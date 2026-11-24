package tierbudget

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/allocate"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/preempt"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_status"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// simPod is a pod in the simulated cluster.
type simPod struct {
	status pod_status.PodStatus // pending, running or releasing
	node   string
	// leaves counts the sessions for which it still releases its GPUs.
	leaves int
}

type simJob struct {
	testJob
	created, startedAt int
	pods               []*simPod
}

func (s *simJob) has(status pod_status.PodStatus) bool {
	for _, p := range s.pods {
		if p.status == status {
			return true
		}
	}
	return false
}

// simulation runs KAI's actions over many sessions. Between sessions it applies what
// the scheduler decided, and jobs arrive and finish at random.
type simulation struct {
	t         *testing.T
	r         *rand.Rand
	args      framework.PluginArguments
	budget    tiers.Units
	nodes     map[string]int
	jobs      []*simJob
	now, next int
	evictions int
	placed    int
}

func (s *simulation) arrive() {
	class := []string{"top", "high", "high", "high", "mid", "mid", "mid", "low", "low"}[s.r.Intn(9)]
	job := &simJob{created: s.now}
	job.testJob = testJob{
		name: fmt.Sprintf("%s-%d", class, s.next), class: class,
		owner: []string{"alice", "bob", "carol"}[s.r.Intn(3)], gpus: float64(1 + s.r.Intn(3)),
	}
	s.next++
	for range 1 + s.r.Intn(2) { // a gang, if two
		job.pods = append(job.pods, &simPod{status: pending})
	}
	s.jobs = append(s.jobs, job)
}

func (s *simulation) finish() {
	kept := s.jobs[:0]
	for _, job := range s.jobs {
		if !(job.has(running) && !job.has(pending) && !job.has(releasing) && s.r.Intn(8) == 0) {
			kept = append(kept, job)
		}
	}
	s.jobs = kept
}

// step runs one session and returns the number of pods evicted.
func (s *simulation) step() int {
	var specs []testJob
	for _, job := range s.jobs {
		spec := job.testJob
		spec.age, spec.started = s.now-job.created+1, s.now-job.startedAt+1
		spec.pods, spec.nodes = nil, nil
		for _, p := range job.pods {
			spec.pods = append(spec.pods, p.status)
			spec.nodes = append(spec.nodes, p.node)
		}
		specs = append(specs, spec)
	}
	ssn := session(s.t, s.nodes, specs)
	p := open(ssn, s.args)
	promoted := map[string]bool{}
	for _, job := range s.jobs {
		promoted[job.name] = p.results[string(find(s.t, ssn, job.name).UID)].Tier == tiers.Promoted
	}
	allocate.New().Execute(ssn)
	NewConsolidation().Execute(ssn)
	preempt.New().Execute(ssn)

	used := map[string]tiers.Units{}
	newP1 := map[string]bool{}
	topPlaced := false
	var evicted []*simJob
	total := 0
	for _, job := range s.jobs {
		info := find(s.t, ssn, job.name)
		if job.class == "high" {
			if u := p.usage(info); u.active {
				used[job.owner] += u.whole()
				newP1[job.owner] = newP1[job.owner] || !job.has(running) && !job.has(releasing)
			}
		}
		wasEvicted := false
		for _, task := range info.GetAllPodsMap() {
			pod := job.pods[podIndex(s.t, task.Name)]
			switch {
			case pod.status == running && task.Status != running:
				// Evicted, if only to be placed elsewhere.
				pod.status, pod.leaves = releasing, 1+s.r.Intn(3)
				wasEvicted = true
				total++
			case pod.status == pending && task.Status == pod_status.Binding || pod.status == pending && task.Status == pod_status.Allocated:
				if !job.has(running) {
					job.startedAt = s.now
				}
				pod.status, pod.node = running, task.NodeName
				s.placed++
				topPlaced = topPlaced || job.class == "top"
			case pod.status == pending && task.Status == pod_status.Pipelined:
				topPlaced = topPlaced || job.class == "top"
			case pod.status == releasing:
				if pod.leaves--; pod.leaves <= 0 {
					pod.status, pod.node = pending, ""
				}
			}
		}
		if wasEvicted {
			evicted = append(evicted, job)
		}
	}
	for owner, u := range used {
		if u > s.budget {
			s.t.Errorf("session %d: %s uses %s of %s", s.now, owner, u, s.budget)
		}
	}
	for _, job := range evicted {
		if (job.class == "high" || promoted[job.name] || job.class == "top") && !topPlaced {
			s.t.Errorf("session %d: %s (promoted=%v) evicted without a top job", s.now, job.name, promoted[job.name])
		}
		if job.class == "top" {
			s.t.Errorf("session %d: %s evicted", s.now, job.name)
		}
	}
	for node, free := range s.nodes {
		for _, job := range s.jobs {
			for _, pod := range job.pods {
				if pod.node == node && pod.status != pending {
					free -= int(job.gpus)
				}
			}
		}
		if free < 0 {
			s.t.Fatalf("session %d: %s is over capacity", s.now, node)
		}
	}
	p.OnSessionClose(ssn)
	s.now++
	s.evictions += total
	return total
}

func podIndex(t *testing.T, name string) int {
	i := len(name)
	for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
		i--
	}
	n, err := strconv.Atoi(name[i:])
	if err != nil {
		t.Fatalf("pod %q", name)
	}
	return n
}

// Over many sessions, budgets hold, protected jobs are evicted only for the exempt
// class, and once nothing arrives or finishes, evictions stop.
// Set SIMULATIONS for a longer run.
func TestSimulation(t *testing.T) {
	seeds := 6
	if n, err := strconv.Atoi(os.Getenv("SIMULATIONS")); err == nil {
		seeds = n
	}
	evictions, placed := 0, 0
	for seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			r := rand.New(rand.NewSource(int64(seed)))
			budget := 1 + r.Intn(5)
			args := testArgs(strconv.Itoa(budget))
			args["allowance"] = strconv.Itoa(r.Intn(5))
			if r.Intn(2) == 0 {
				args["consolidation"] = "top,high>mid,low;mid,low>mid,low"
			}
			s := &simulation{t: t, r: r, args: args, budget: tiers.UnitsOf(float64(budget)), nodes: map[string]int{"n0": 4, "n1": 4, "n2": 4}}
			for range 25 {
				for range r.Intn(3) {
					s.arrive()
				}
				s.finish()
				s.step()
			}
			quiet, steps := 0, 0
			for ; quiet < 8 && steps < 80; steps++ {
				if s.step() == 0 {
					quiet++
				} else {
					quiet = 0
				}
			}
			if quiet < 8 {
				t.Errorf("still evicting after %d sessions without changes", steps)
			}
			evictions, placed = evictions+s.evictions, placed+s.placed
		})
	}
	t.Logf("%d pods placed, %d evicted", placed, evictions)
	if evictions < seeds || placed < 10*seeds {
		t.Error("too little happened")
	}
}

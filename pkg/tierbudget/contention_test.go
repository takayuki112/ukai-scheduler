package tierbudget

import (
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"testing"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/allocate"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/preempt"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_status"
)

// contention runs sessions with only P1 jobs, which are given oldest first. In each, the
// jobs that started longest ago end, up to ends of them. It returns the jobs in the order
// in which they started. Those that start in one session are in order of age.
func contention(t *testing.T, nodes map[string]int, budget, ends int, jobs []testJob) []string {
	t.Helper()
	var order []string
	for round := 0; len(order) < len(jobs); round++ {
		if round > 10*len(jobs)+10 {
			t.Fatalf("stuck after %v", order)
		}
		ended := 0
		for _, name := range order {
			i := slices.IndexFunc(jobs, func(j testJob) bool { return j.name == name })
			if jobs[i].pods != nil && ended < ends {
				jobs[i].pods = nil
				ended++
			}
		}
		var specs []testJob
		for _, job := range jobs {
			if job.pods != nil {
				specs = append(specs, job)
			}
		}
		ssn := session(t, nodes, specs)
		p := open(ssn, testArgs(strconv.Itoa(budget)))
		allocate.New().Execute(ssn)
		NewConsolidation().Execute(ssn)
		preempt.New().Execute(ssn)
		used := map[string]float64{}
		for i, job := range jobs {
			if job.pods == nil {
				continue
			}
			info := find(t, ssn, job.name)
			switch s := state(info); {
			case s == "placed":
				var at []string
				for _, task := range info.GetAllPodsMap() {
					at = append(at, task.NodeName)
				}
				jobs[i].pods, jobs[i].nodes = []pod_status.PodStatus{running}, at
				order = append(order, job.name)
				used[job.owner] += job.gpus
			case s == "running":
				used[job.owner] += job.gpus
			case s != "pending":
				t.Fatalf("%s is %s: a P1 must not evict another", job.name, s)
			}
		}
		for owner, u := range used {
			if u > float64(budget) {
				t.Errorf("%s uses %v", owner, u)
			}
		}
		p.OnSessionClose(ssn)
	}
	return order
}

func TestContention(t *testing.T) {
	one := map[string]int{"n0": 8}
	t.Run("by submission, if each user has one job", func(t *testing.T) {
		for seed := range int64(20) {
			r := rand.New(rand.NewSource(seed))
			var jobs []testJob
			var want []string
			for i, user := range r.Perm(10) {
				name := fmt.Sprint("job", i)
				jobs, want = append(jobs, j(name, fmt.Sprint("user", user), "high", 4, pending, 100-i)), append(want, name)
			}
			if got := contention(t, one, 4, 1, jobs); !slices.Equal(got, want) {
				t.Errorf("started %v", got)
			}
		}
	})
	t.Run("a held job does not block those behind it", func(t *testing.T) {
		got := contention(t, one, 4, 1, []testJob{
			j("a1", "alice", "high", 4, pending, 9), j("a2", "alice", "high", 4, pending, 8),
			j("b1", "bob", "high", 4, pending, 7), j("c1", "carol", "high", 4, pending, 6),
		})
		if want := []string{"a1", "b1", "a2", "c1"}; !slices.Equal(got, want) {
			t.Errorf("started %v, want %v", got, want)
		}
	})
	t.Run("a backlog goes before later jobs of others", func(t *testing.T) {
		got := contention(t, map[string]int{"n0": 4}, 4, 1, []testJob{
			j("a1", "alice", "high", 4, pending, 9), j("a2", "alice", "high", 4, pending, 8), j("a3", "alice", "high", 4, pending, 7),
			j("b1", "bob", "high", 4, pending, 6),
		})
		if want := []string{"a1", "a2", "a3", "b1"}; !slices.Equal(got, want) {
			t.Errorf("started %v, want %v", got, want)
		}
	})
	t.Run("small jobs overtake one that does not fit the free GPUs", func(t *testing.T) {
		jobs := []testJob{j("x1", "u1", "high", 4, pending, 99), j("x2", "u2", "high", 4, pending, 98), j("big", "alice", "high", 8, pending, 97)}
		want := []string{"x1", "x2"}
		for i := range 8 {
			name := fmt.Sprint("small", i)
			jobs, want = append(jobs, j(name, fmt.Sprint("s", i), "high", 4, pending, 90-i)), append(want, name)
		}
		if got := contention(t, one, 8, 1, jobs); !slices.Equal(got, append(want, "big")) {
			t.Errorf("started %v", got)
		}
	})
}

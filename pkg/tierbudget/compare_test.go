package tierbudget

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/allocate"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/consolidation"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/preempt"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_status"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/podgroup_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"
)

// The comparison runs KAI's actions and this plugin against a simulated clock, with
// heavy users who always have work queued and light users who now and then need a GPU
// or two. It is slow, so it runs only when asked:
//
//	COMPARE=1 go test ./pkg/tierbudget -run TestCompare -v -timeout 2h
//
// COMPARE_HOURS (default 48), COMPARE_SEEDS (default 3) and COMPARE_STEP (default 2m)
// change its length. Jobs have one pod and no checkpoints: an evicted job starts over.
func TestCompare(t *testing.T) {
	if os.Getenv("COMPARE") == "" {
		t.Skip("set COMPARE=1 to run")
	}
	hours := envInt("COMPARE_HOURS", 48)
	seeds := envInt("COMPARE_SEEDS", 3)
	step, err := time.ParseDuration(cmpEnv("COMPARE_STEP", "2m"))
	if err != nil {
		t.Fatal(err)
	}
	only := os.Getenv("COMPARE_ONLY")

	var rows []simResult
	for _, sc := range simSchemes() {
		if only != "" && !strings.Contains(only, sc.name) {
			continue
		}
		var runs []simResult
		for seed := range seeds {
			start := time.Now()
			r := simulate(t, sc, int64(seed+1), time.Duration(hours)*time.Hour, step)
			t.Logf("%s seed %d: %s", sc.name, seed+1, time.Since(start).Round(time.Second))
			runs = append(runs, r)
		}
		rows = append(rows, average(sc.name, runs))
	}
	t.Logf("\n%s", simTable(rows, hours, seeds))
}

type simScheme struct {
	name string
	args framework.PluginArguments // nil for stock KAI
	// class says which class a user's new job has, by what the user has.
	class   func(s *sim, user string, gpus float64) string
	protect bool
}

func simSchemes() []simScheme {
	mid := func(*sim, string, float64) string { return "mid" }
	budget := testArgs("8")
	budget["allowance"] = "8"
	ladderBase := map[string]string{
		"ladderClasses": "mid", "levelLabel": "", "whenFullLabel": "example.com/when-full", "evictGap": "10",
		"protectLabel": "", "protectLimit": "",
	}
	units := ladderArgs(ladderBase)
	units["ladder"] = "75=8, 70=8, 65=8, 60=8, 55=16, 40"
	share := ladderArgs(ladderBase)
	share["ladderWidth"] = "share"
	share["ladder"] = "75=0.25, 70=0.25, 65=0.25, 60=0.25, 55=0.5, 40"
	protect := ladderArgs(ladderBase)
	protect["ladderWidth"], protect["ladder"] = share["ladderWidth"], share["ladder"]
	protect["protectLabel"], protect["protectLimit"] = "example.com/protect", "3"
	for _, a := range []framework.PluginArguments{units, share, protect} {
		for k, v := range a {
			if v == "" {
				delete(a, k)
			}
		}
	}
	return []simScheme{
		{name: "stock", class: mid},
		{name: "budget", args: budget, class: func(s *sim, user string, gpus float64) string {
			// Users run P1 within their budget, and P2 beyond it.
			if s.unitsOf(user, "high")+gpus <= 8 {
				return "high"
			}
			return "mid"
		}},
		{name: "ladder", args: units, class: mid},
		{name: "share", args: share, class: mid},
		{name: "share+protect", args: protect, class: mid, protect: true},
	}
}

type cmpJob struct {
	name, user, class string
	heavy, protect    bool
	gpus              float64
	work              time.Duration
	created           time.Time
	started           time.Time // of the current run; zero if not running
	firstStart        time.Time
	node              string
	done              time.Time
	evictions         int
	lostGPUHours      float64
}

type sim struct {
	jobs []*cmpJob
	next int
}

func (s *sim) unitsOf(user, class string) float64 {
	var u float64
	for _, j := range s.jobs {
		if j.user == user && j.class == class && j.done.IsZero() {
			u += j.gpus
		}
	}
	return u
}

const (
	simNodes      = 8
	simGPUs       = 8
	simHeavy      = 2
	simLight      = 10
	simHeavyQueue = 3                   // pending jobs each heavy user keeps
	simLightEvery = 4 * time.Hour       // mean time between a light user's jobs
	simWarmup     = 6 * time.Hour       // jobs submitted before are not measured
	simTail       = 6 * time.Hour       // nor those submitted in the last hours
	simBase       = 1_700_000_000 * 1e9 // a fixed start, so runs are equal
)

func simulate(t *testing.T, sc simScheme, seed int64, length, step time.Duration) simResult {
	r := rand.New(rand.NewSource(seed))
	s := &sim{}
	start := time.Unix(0, simBase)
	now := start
	defer func() { clock = time.Now }()
	resetActiveUsers()

	submit := func(user string, heavy bool) {
		j := &cmpJob{name: fmt.Sprintf("j%d", s.next), user: user, heavy: heavy, created: now}
		s.next++
		if heavy {
			j.gpus = []float64{4, 8}[r.Intn(2)]
			j.work = time.Duration(2+r.Intn(9)) * time.Hour
			j.protect = sc.protect && r.Intn(2) == 0
		} else {
			j.gpus = []float64{1, 1, 1, 2, 2}[r.Intn(5)]
			j.work = time.Duration(30+r.Intn(151)) * time.Minute
		}
		j.class = sc.class(s, user, j.gpus)
		s.jobs = append(s.jobs, j)
	}

	var busy float64
	var samples int
	nodes := map[string]int{}
	for i := range simNodes {
		nodes[fmt.Sprintf("n%d", i)] = simGPUs
	}
	for now.Before(start.Add(length)) {
		now = now.Add(step)
		// Ends.
		for _, j := range s.jobs {
			if !j.started.IsZero() && j.done.IsZero() && now.Sub(j.started) >= j.work {
				j.done, j.started, j.node = now, time.Time{}, ""
			}
		}
		// Arrivals.
		for h := range simHeavy {
			user := fmt.Sprintf("heavy%d", h)
			waiting := 0
			for _, j := range s.jobs {
				if j.user == user && j.done.IsZero() && j.started.IsZero() {
					waiting++
				}
			}
			for ; waiting < simHeavyQueue; waiting++ {
				submit(user, true)
			}
		}
		for l := range simLight {
			if r.Float64() < float64(step)/float64(simLightEvery) {
				submit(fmt.Sprintf("light%d", l), false)
			}
		}

		// One scheduling cycle.
		var specs []testJob
		live := map[string]*cmpJob{}
		for _, j := range s.jobs {
			if !j.done.IsZero() {
				continue
			}
			live[j.name] = j
			tj := testJob{name: j.name, user: j.user, class: j.class, gpus: j.gpus, pods: []pod_status.PodStatus{pending}, age: 1}
			if !j.started.IsZero() {
				tj.pods, tj.node = []pod_status.PodStatus{running}, j.node
			}
			specs = append(specs, tj)
		}
		ssn := session(t, nodes, specs)
		infos := map[string]*podgroup_info.PodGroupInfo{}
		for _, info := range ssn.ClusterInfo.PodGroupInfos {
			j := live[info.Name]
			if j == nil {
				continue
			}
			infos[info.Name] = info
			info.CreationTimestamp = metav1.Time{Time: j.created}
			info.PodGroup.CreationTimestamp = info.CreationTimestamp
			if !j.started.IsZero() {
				started := j.started
				info.LastStartTimestamp = &started
			}
			if j.protect {
				info.PodGroup.Labels["example.com/protect"] = "true"
				info.PodGroup.Labels["example.com/when-full"] = "lower"
			}
		}
		clock = func() time.Time { return now }
		var p *plugin
		if sc.args != nil {
			p = open(ssn, sc.args)
		}
		allocate.New().Execute(ssn)
		if p != nil {
			NewConsolidation().Execute(ssn)
		} else {
			consolidation.New().Execute(ssn)
		}
		preempt.New().Execute(ssn)
		if p != nil {
			p.OnSessionClose(ssn)
		}

		var used float64
		for name, info := range infos {
			j := live[name]
			for _, task := range info.GetAllPodsMap() {
				switch {
				case task.Status == pod_status.Releasing && !j.started.IsZero():
					j.evictions++
					j.lostGPUHours += j.gpus * now.Sub(j.started).Hours()
					j.started, j.node = time.Time{}, ""
				case task.Status == pod_status.Running:
					used += j.gpus
				case pod_status.IsActiveAllocatedStatus(task.Status) && j.started.IsZero():
					j.started, j.node = now, task.NodeName
					if j.firstStart.IsZero() {
						j.firstStart = now
					}
					used += j.gpus
				}
			}
		}
		busy += used
		samples++
	}
	return measure(s, start, now, busy/float64(samples))
}

type simResult struct {
	name                         string
	lightWait50, lightWait95     float64 // minutes
	heavyWait50                  float64 // minutes
	lightEvictions, heavyEvicted float64
	lostGPUHours                 float64
	heavyGPUHours, lightGPUHours float64 // of finished work
	lightUnstarted               float64
	busy                         float64 // share of GPUs
}

func measure(s *sim, start, end time.Time, busy float64) simResult {
	res := simResult{busy: busy / (simNodes * simGPUs)}
	var light, heavy []float64
	for _, j := range s.jobs {
		if j.created.Before(start.Add(simWarmup)) || j.created.After(end.Add(-simTail)) {
			continue
		}
		wait := end.Sub(j.created) // not started: waited at least this long
		if !j.firstStart.IsZero() {
			wait = j.firstStart.Sub(j.created)
		} else if !j.heavy {
			res.lightUnstarted++
		}
		if j.heavy {
			heavy = append(heavy, wait.Minutes())
			res.heavyEvicted += float64(j.evictions)
		} else {
			light = append(light, wait.Minutes())
			res.lightEvictions += float64(j.evictions)
		}
		res.lostGPUHours += j.lostGPUHours
	}
	for _, j := range s.jobs {
		if !j.done.IsZero() {
			if j.heavy {
				res.heavyGPUHours += j.gpus * j.work.Hours()
			} else {
				res.lightGPUHours += j.gpus * j.work.Hours()
			}
		}
	}
	res.lightWait50, res.lightWait95 = percentile(light, 50), percentile(light, 95)
	res.heavyWait50 = percentile(heavy, 50)
	return res
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	v = slices.Clone(v)
	slices.Sort(v)
	return v[int(math.Ceil(p/100*float64(len(v))))-1]
}

func average(name string, runs []simResult) simResult {
	a := simResult{name: name}
	for _, r := range runs {
		a.lightWait50 += r.lightWait50
		a.lightWait95 += r.lightWait95
		a.heavyWait50 += r.heavyWait50
		a.lightEvictions += r.lightEvictions
		a.heavyEvicted += r.heavyEvicted
		a.lostGPUHours += r.lostGPUHours
		a.heavyGPUHours += r.heavyGPUHours
		a.lightGPUHours += r.lightGPUHours
		a.lightUnstarted += r.lightUnstarted
		a.busy += r.busy
	}
	n := float64(len(runs))
	for _, f := range []*float64{&a.lightWait50, &a.lightWait95, &a.heavyWait50, &a.lightEvictions, &a.heavyEvicted,
		&a.lostGPUHours, &a.heavyGPUHours, &a.lightGPUHours, &a.lightUnstarted, &a.busy} {
		*f /= n
	}
	return a
}

func simTable(rows []simResult, hours, seeds int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d nodes of %d GPUs, %d heavy and %d light users, %d hours, mean of %d seeds\n\n",
		simNodes, simGPUs, simHeavy, simLight, hours, seeds)
	b.WriteString("| Scheme | Light wait, median | Light wait, 95% | Light never started | Heavy wait, median | Evictions of light | Evictions of heavy | GPU hours lost | Heavy GPU hours | Light GPU hours | GPUs busy |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %.1f | %s | %.1f | %.1f | %.0f | %.0f | %.0f | %.0f%% |\n",
			r.name, minutes(r.lightWait50), minutes(r.lightWait95), r.lightUnstarted, minutes(r.heavyWait50),
			r.lightEvictions, r.heavyEvicted, r.lostGPUHours, r.heavyGPUHours, r.lightGPUHours, 100*r.busy)
	}
	return b.String()
}

func minutes(m float64) string {
	if m < 90 {
		return fmt.Sprintf("%.0f min", m)
	}
	return fmt.Sprintf("%.1f h", m/60)
}

func envInt(key string, otherwise int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return otherwise
	}
	return n
}

func cmpEnv(key, otherwise string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return otherwise
}

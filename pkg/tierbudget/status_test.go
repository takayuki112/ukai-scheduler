package tierbudget

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"
	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// Budget 2, allowance 4.
var statusJobs = []testJob{
	j("run", "alice", "high", 1, running, 9), j("held", "alice", "high", 2, pending, 8),
	j("promoted", "alice", "mid", 1, running, 7), j("normal", "alice", "mid", 4, pending, 6),
	j("excess", "bob", "mid", 8, running, 5), j("waits", "bob", "high", 0.5, pending, 4),
	j("top", "bob", "top", 1, running, 3), j("nogpu", "bob", "high", 0, running, 3),
}

func get(t *testing.T, path string) (Status, string) {
	t.Helper()
	w := httptest.NewRecorder()
	serveStatus(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body)
	}
	var s Status
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s, w.Body.String()
}

func TestStatus(t *testing.T) {
	want := map[string]JobStatus{
		"run":      {User: "alice", Class: "high", Running: true, Units: 1, Tier: tiers.Budget, Priority: 75, ClassPriority: 75},
		"held":     {User: "alice", Class: "high", Units: 2, Tier: tiers.Held, Priority: 75, ClassPriority: 75, Reason: "needs 2 units, using 1 of 2"},
		"promoted": {User: "alice", Class: "mid", Running: true, Units: 1, Tier: tiers.Promoted, Priority: 75, ClassPriority: 50},
		"normal":   {User: "alice", Class: "mid", Units: 4, Tier: tiers.Normal, Priority: 50, ClassPriority: 50},
		"excess":   {User: "bob", Class: "mid", Running: true, Units: 8, Tier: tiers.Excess, Priority: 45, ClassPriority: 50},
		"waits":    {User: "bob", Class: "high", Units: 0.5, Tier: tiers.Budget, Priority: 75, ClassPriority: 75},
	}
	for _, mode := range []string{"audit", "enforce"} {
		args := testArgs("2")
		args["mode"] = mode
		ssn := session(t, map[string]int{"n0": 16}, statusJobs)
		for name, w := range want {
			w.Namespace, w.Name = find(t, ssn, name).Namespace, name
			want[name] = w
		}
		open(ssn, args)
		s, _ := get(t, StatusPath)
		if s.Mode != Mode(mode) || s.Error != "" || s.Budget != 2 || s.Time.IsZero() || len(s.Jobs) != len(want) {
			t.Errorf("%s: %+v", mode, s)
		}
		for _, job := range s.Jobs {
			if job != want[job.Name] {
				t.Errorf("%s: got %+v, want %+v", mode, job, want[job.Name])
			}
		}
		if bob, _ := get(t, StatusPath+"?user=bob"); len(bob.Jobs) != 2 || bob.Jobs[0].User != "bob" {
			t.Errorf("bob: %+v", bob.Jobs)
		}
		if _, body := get(t, StatusPath+"?user=nobody"); !strings.Contains(body, `"jobs": []`) {
			t.Errorf("nobody: %s", body)
		}
	}
}

func TestMetrics(t *testing.T) {
	ssn := session(t, map[string]int{"n0": 16}, statusJobs)
	args := testArgs("2")
	args["jobMetrics"] = "true"
	ns := find(t, ssn, "run").Namespace
	open(ssn, args)
	want := `
# HELP ukai_active 1 if the plugin works, 0 if stock behaviour applies because of an error
# TYPE ukai_active gauge
ukai_active{mode="enforce"} 1
# HELP ukai_user_budget_units Budget of each user
# TYPE ukai_user_budget_units gauge
ukai_user_budget_units 2
# HELP ukai_jobs Jobs by effective tier
# TYPE ukai_jobs gauge
ukai_jobs{state="pending",tier="held",user="alice"} 1
ukai_jobs{state="pending",tier="normal",user="alice"} 1
ukai_jobs{state="running",tier="budget",user="alice"} 1
ukai_jobs{state="running",tier="promoted",user="alice"} 1
ukai_jobs{state="pending",tier="budget",user="bob"} 1
ukai_jobs{state="running",tier="excess",user="bob"} 1
# HELP ukai_units Units of jobs by effective tier
# TYPE ukai_units gauge
ukai_units{state="pending",tier="held",user="alice"} 2
ukai_units{state="pending",tier="normal",user="alice"} 4
ukai_units{state="running",tier="budget",user="alice"} 1
ukai_units{state="running",tier="promoted",user="alice"} 1
ukai_units{state="pending",tier="budget",user="bob"} 0.5
ukai_units{state="running",tier="excess",user="bob"} 8
# HELP ukai_job_priority Effective priority of each job
# TYPE ukai_job_priority gauge
ukai_job_priority{namespace="NS",podgroup="held",tier="held",user="alice"} 75
ukai_job_priority{namespace="NS",podgroup="normal",tier="normal",user="alice"} 50
ukai_job_priority{namespace="NS",podgroup="promoted",tier="promoted",user="alice"} 75
ukai_job_priority{namespace="NS",podgroup="run",tier="budget",user="alice"} 75
ukai_job_priority{namespace="NS",podgroup="excess",tier="excess",user="bob"} 45
ukai_job_priority{namespace="NS",podgroup="waits",tier="budget",user="bob"} 75
`
	names := []string{"ukai_active", "ukai_user_budget_units", "ukai_jobs", "ukai_units", "ukai_job_priority"}
	if err := testutil.CollectAndCompare(collector{}, strings.NewReader(strings.ReplaceAll(want, "NS", ns)), names...); err != nil {
		t.Error(err)
	}
	if n := testutil.CollectAndCount(collector{}, "ukai_last_session_timestamp_seconds"); n != 1 {
		t.Errorf("%d timestamps", n)
	}

	// Jobs that are gone leave nothing behind, and job metrics are off by default.
	open(session(t, map[string]int{"n0": 16}, statusJobs[:1]), testArgs("2"))
	if n := testutil.CollectAndCount(collector{}, "ukai_jobs", "ukai_units", "ukai_job_priority"); n != 2 {
		t.Errorf("%d series, want 2", n)
	}
	// They are on the registry that KAI serves.
	if n, err := testutil.GatherAndCount(prometheus.DefaultGatherer, "ukai_active"); n != 1 || err != nil {
		t.Error(n, err)
	}
}

func TestTierChanges(t *testing.T) {
	lastStatus.Store(nil)
	count := func(from, to tiers.Tier) float64 {
		return testutil.ToFloat64(tierChanges.WithLabelValues(string(from), string(to)))
	}
	up, down := count(tiers.Normal, tiers.Promoted), count(tiers.Promoted, tiers.Normal)
	p2 := j("a", "alice", "mid", 2, running, 9)
	p1 := j("b", "alice", "high", 2, running, 8)
	for _, jobs := range [][]testJob{{p2}, {p2}, {p2, p1}, {p2, p1}, {p2}, {}, {p2, p1}} {
		open(session(t, map[string]int{"n0": 8}, jobs), testArgs("2"))
	}
	if got := count(tiers.Promoted, tiers.Normal) - down; got != 1 {
		t.Errorf("%v demotions, want 1", got)
	}
	if got := count(tiers.Normal, tiers.Promoted) - up; got != 1 {
		t.Errorf("%v promotions, want 1", got)
	}
	if got := count(tiers.Promoted, tiers.Promoted); got != 0 {
		t.Errorf("%v changes to the same tier", got)
	}
}

func TestStatusOfFailures(t *testing.T) {
	lastStatus.Store(nil)
	w := httptest.NewRecorder()
	serveStatus(w, httptest.NewRequest("GET", StatusPath, nil))
	if w.Code != 503 || testutil.CollectAndCount(collector{}) != 0 {
		t.Errorf("before the first session: %d", w.Code)
	}

	check := func(text string) {
		t.Helper()
		s, _ := get(t, StatusPath)
		if !strings.Contains(s.Error, text) || len(s.Jobs) != 0 {
			t.Errorf("%+v", s)
		}
		if n := testutil.CollectAndCount(collector{}, "ukai_jobs", "ukai_user_budget_units"); n != 0 {
			t.Errorf("%d series", n)
		}
	}
	good := session(t, map[string]int{"n0": 16}, statusJobs)
	open(good, testArgs("2"))
	open(session(t, map[string]int{"n0": 16}, statusJobs), framework.PluginArguments{"mode": "x"})
	check("invalid configuration: mode")

	open(good, testArgs("2"))
	before := testutil.ToFloat64(panicCount)
	broken := session(t, map[string]int{"n0": 16}, statusJobs)
	broken.ClusterInfo = nil
	open(broken, testArgs("2"))
	check("panic")
	if got := testutil.ToFloat64(panicCount) - before; got != 1 {
		t.Errorf("%v panics counted", got)
	}
	if !strings.Contains(string(mustJSON(t)), `"mode":"enforce"`) {
		t.Error("no mode")
	}

	// A panic while collecting must not end the process.
	ch := make(chan prometheus.Metric)
	close(ch)
	collector{}.Collect(ch)
}

func mustJSON(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(lastStatus.Load())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// For the race detector.
func TestReadDuringSessions(t *testing.T) {
	var wg sync.WaitGroup
	done := make(chan struct{})
	open(session(t, map[string]int{"n0": 16}, statusJobs), testArgs("2"))
	for range 2 {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					testutil.CollectAndCount(collector{})
					serveStatus(httptest.NewRecorder(), httptest.NewRequest("GET", StatusPath+"?user=bob", nil))
				}
			}
		})
	}
	for range 10 {
		schedule(session(t, map[string]int{"n0": 16}, statusJobs), testArgs("2"))
	}
	close(done)
	wg.Wait()
}

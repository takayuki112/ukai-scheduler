package ladder

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

var t0 = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

func u(v float64) tiers.Units { return tiers.UnitsOf(v) }

// policy is the ladder of the design: 75=4, 70=4, 65=4, 60=4, 55=8, 40.
func policy() Policy {
	return Policy{
		Rungs: []Rung{
			{75, u(4)}, {70, u(4)}, {65, u(4)}, {60, u(4)}, {55, u(8)}, {40, 0},
		},
		ProtectCost:  3,
		ProtectLimit: u(12),
		ProtectFor:   24 * time.Hour,
	}
}

func running(id, user string, gpus float64, minute int) Job {
	at := t0.Add(time.Duration(minute) * time.Minute)
	return Job{ID: id, User: user, Units: u(gpus), Running: true, Created: at, Started: at}
}

func pending(id, user string, gpus float64, minute int) Job {
	return Job{ID: id, User: user, Units: u(gpus), Created: t0.Add(time.Duration(minute) * time.Minute)}
}

type want struct {
	tier     tiers.Tier
	priority int32
}

func check(t *testing.T, got map[string]Result, expect map[string]want) {
	t.Helper()
	for id, w := range expect {
		r, ok := got[id]
		if !ok {
			t.Errorf("%s: no result", id)
			continue
		}
		if r.Tier != w.tier || (w.tier != Held && r.Priority != w.priority) {
			t.Errorf("%s: got %s %d (%s), want %s %d", id, r.Tier, r.Priority, r.Reason, w.tier, w.priority)
		}
	}
	if len(got) != len(expect) {
		t.Errorf("got %d results, want %d", len(got), len(expect))
	}
}

func TestSteps(t *testing.T) {
	jobs := []Job{
		running("A", "alice", 2, 0), running("B", "alice", 2, 1), running("C", "alice", 2, 2),
		running("D", "alice", 2, 3), running("E", "alice", 2, 4),
	}
	check(t, Assign(policy(), jobs, t0.Add(time.Hour)), map[string]want{
		"A": {Step, 75}, "B": {Step, 75}, "C": {Step, 70}, "D": {Step, 70}, "E": {Step, 65},
	})
}

func TestTheJobEndsWhereItsLastUnitIs(t *testing.T) {
	jobs := []Job{running("A", "alice", 3, 0), running("B", "alice", 2, 1), running("C", "alice", 30, 2)}
	check(t, Assign(policy(), jobs, t0), map[string]want{
		"A": {Step, 75}, "B": {Step, 70}, "C": {Step, 40},
	})
}

func TestUsersAreSeparate(t *testing.T) {
	jobs := []Job{running("A", "alice", 8, 0), running("B", "alice", 4, 1), running("X", "bob", 4, 2)}
	check(t, Assign(policy(), jobs, t0), map[string]want{
		"A": {Step, 70}, "B": {Step, 65}, "X": {Step, 75},
	})
}

func TestPendingGoesLastAndRunningKeepsItsStep(t *testing.T) {
	jobs := []Job{pending("P", "alice", 4, 0), running("A", "alice", 4, 5), running("B", "alice", 4, 6)}
	check(t, Assign(policy(), jobs, t0), map[string]want{
		"A": {Step, 75}, "B": {Step, 70}, "P": {Step, 65},
	})
}

func TestALevelGoesFirstAndPushesOthersDown(t *testing.T) {
	jobs := []Job{
		running("A", "alice", 2, 0), running("B", "alice", 2, 1), running("C", "alice", 2, 2),
		running("D", "alice", 2, 3),
	}
	f := pending("F", "alice", 2, 10)
	f.Level = 75
	jobs = append(jobs, f)
	check(t, Assign(policy(), jobs, t0), map[string]want{
		"F": {Level, 75}, "A": {Step, 75}, "B": {Step, 70}, "C": {Step, 70}, "D": {Step, 65},
	})
}

func TestALevelThatDoesNotFitWaits(t *testing.T) {
	f1, f2, f3 := pending("F1", "alice", 4, 0), pending("F2", "alice", 2, 1), pending("F3", "alice", 2, 2)
	f1.Level, f2.Level, f3.Level = 75, 75, 70
	got := Assign(policy(), []Job{f1, f2, f3}, t0)
	check(t, got, map[string]want{"F1": {Level, 75}, "F2": {Held, 0}, "F3": {Level, 70}})
	if got["F2"].Reason != "level 75: needs 2 units, using 4 of 4" {
		t.Errorf("reason: %q", got["F2"].Reason)
	}
}

func TestALevelCountsOnlyThoseAtLeastAsHigh(t *testing.T) {
	low, high := running("L", "alice", 8, 0), pending("H", "alice", 4, 1)
	low.Level, high.Level = 70, 75
	check(t, Assign(policy(), []Job{low, high}, t0), map[string]want{"L": {Level, 70}, "H": {Level, 75}})
}

func TestARunningLevelIsKeptOverTheCap(t *testing.T) {
	a, b := running("A", "alice", 4, 0), running("B", "alice", 4, 1)
	a.Level, b.Level = 75, 75
	check(t, Assign(policy(), []Job{a, b}, t0), map[string]want{"A": {Level, 75}, "B": {Level, 75}})
}

func TestLowerTakesTheHighestRungWithRoom(t *testing.T) {
	a, b := running("A", "alice", 4, 0), pending("B", "alice", 4, 1)
	a.Level, b.Level, b.Lower = 75, 75, true
	check(t, Assign(policy(), []Job{a, b}, t0), map[string]want{"A": {Level, 75}, "B": {Level, 70}})
}

func TestALevelAboveTheTopWaits(t *testing.T) {
	a := pending("A", "alice", 1, 0)
	a.Level = 90
	check(t, Assign(policy(), []Job{a}, t0), map[string]want{"A": {Held, 0}})
}

func TestALevelAtTheFloorAlwaysFits(t *testing.T) {
	a := pending("A", "alice", 100, 0)
	a.Level = 40
	check(t, Assign(policy(), []Job{a}, t0), map[string]want{"A": {Level, 40}})
}

func TestProtectionCostsMore(t *testing.T) {
	p := running("P", "alice", 2, 0)
	p.Protect = true
	jobs := []Job{p, running("A", "alice", 2, 1), running("B", "alice", 2, 2)}
	got := Assign(policy(), jobs, t0.Add(time.Hour))
	// 2 units at a cost of 3 take 6: A ends at 8, B at 10.
	check(t, got, map[string]want{"P": {Protected, 75}, "A": {Step, 70}, "B": {Step, 65}})
	if !got["P"].NonPreemptible || got["A"].NonPreemptible {
		t.Error("only P should be non-preemptible")
	}
}

func TestProtectionLimitPerUser(t *testing.T) {
	p1, p2, p3 := pending("P1", "alice", 2, 0), pending("P2", "alice", 2, 1), pending("P3", "alice", 1, 2)
	p1.Protect, p2.Protect, p3.Protect = true, true, true
	p3.Lower = true
	got := Assign(policy(), []Job{p1, p2, p3}, t0)
	// Costs 6 and 6 fill the limit of 12. P3 goes on as an ordinary job.
	check(t, got, map[string]want{"P1": {Protected, 75}, "P2": {Protected, 75}, "P3": {Step, 60}})

	p4 := pending("P4", "alice", 1, 3)
	p4.Protect = true
	got = Assign(policy(), []Job{p1, p2, p4}, t0)
	if got["P4"].Tier != Held || got["P4"].Reason != "protection: costs 3 units, using 12 of 12" {
		t.Errorf("P4: %+v", got["P4"])
	}
}

func TestProtectionLimitForTheCluster(t *testing.T) {
	pol := policy()
	pol.ProtectCluster = u(4)
	a, b, c := running("A", "alice", 2, 0), pending("B", "bob", 2, 1), pending("C", "carol", 1, 2)
	a.Protect, b.Protect, c.Protect = true, true, true
	got := Assign(pol, []Job{a, b, c}, t0)
	check(t, got, map[string]want{"A": {Protected, 75}, "B": {Protected, 75}, "C": {Held, 0}})
}

func TestARunningProtectedJobStaysOverTheLimit(t *testing.T) {
	pol := policy()
	pol.ProtectLimit = u(3)
	a := running("A", "alice", 2, 0)
	a.Protect = true
	check(t, Assign(pol, []Job{a}, t0), map[string]want{"A": {Protected, 75}})
}

func TestProtectionEnds(t *testing.T) {
	a := running("A", "alice", 2, 0)
	a.Protect = true
	got := Assign(policy(), []Job{a, running("B", "alice", 2, 1)}, t0.Add(24*time.Hour))
	check(t, got, map[string]want{"A": {Step, 75}, "B": {Step, 75}})
	if got["A"].NonPreemptible {
		t.Error("protection should have ended")
	}
}

func TestNoUserGetsTheFloorAndNoProtection(t *testing.T) {
	a := running("A", "", 1, 0)
	a.Protect = true
	got := Assign(policy(), []Job{a}, t0)
	check(t, got, map[string]want{"A": {Step, 40}})
}

func TestNoGPUsIsUnmanaged(t *testing.T) {
	check(t, Assign(policy(), []Job{running("A", "alice", 0, 0)}, t0), map[string]want{"A": {tiers.Unmanaged, 0}})
}

func TestScale(t *testing.T) {
	fractions := []Rung{{75, u(0.25)}, {70, u(0.25)}, {40, 0}}
	got := Scale(fractions, u(16))
	if got[0].Width != u(4) || got[1].Width != u(4) || fractions[0].Width != u(0.25) {
		t.Errorf("got %+v, fractions %+v", got, fractions)
	}
}

// TestProperties checks rules that hold for any jobs.
func TestProperties(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	pol := policy()
	pol.ProtectCluster = u(10)
	for round := range 2000 {
		var jobs []Job
		for i := range r.Intn(12) {
			j := Job{
				ID: fmt.Sprint(i), User: []string{"", "alice", "bob"}[r.Intn(3)],
				Units: u(float64(r.Intn(6))), Running: r.Intn(2) == 0,
				Created: t0.Add(time.Duration(r.Intn(60)) * time.Minute),
				Protect: r.Intn(5) == 0, Lower: r.Intn(2) == 0,
			}
			j.Started = j.Created.Add(time.Duration(r.Intn(30)) * time.Hour)
			if r.Intn(4) == 0 {
				j.Level = []int32{40, 55, 60, 65, 70, 75, 90}[r.Intn(7)]
			}
			jobs = append(jobs, j)
		}
		now := t0.Add(30 * time.Hour)
		got := Assign(pol, jobs, now)
		shuffled := append([]Job(nil), jobs...)
		r.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		again := Assign(pol, shuffled, now)

		var protectedUnits tiers.Units
		for _, j := range jobs {
			res := got[j.ID]
			if res != again[j.ID] {
				t.Fatalf("round %d: order matters for %s: %+v vs %+v", round, j.ID, res, again[j.ID])
			}
			if res.Tier == Held && j.Running {
				t.Fatalf("round %d: running job %s held", round, j.ID)
			}
			if res.Tier == Held && j.Lower {
				t.Fatalf("round %d: job %s with Lower held", round, j.ID)
			}
			if res.Tier != tiers.Unmanaged && res.Tier != Held && (res.Priority < 40 || res.Priority > 75) {
				t.Fatalf("round %d: job %s priority %d off the ladder", round, j.ID, res.Priority)
			}
			if res.NonPreemptible != (res.Tier == Protected) {
				t.Fatalf("round %d: job %s: %+v", round, j.ID, res)
			}
			if res.Tier == Protected && !j.Running {
				protectedUnits += j.Units
			}
		}
		var runningProtected tiers.Units
		for _, j := range jobs {
			if got[j.ID].Tier == Protected && j.Running {
				runningProtected += j.Units
			}
		}
		if protectedUnits > 0 && runningProtected+protectedUnits > pol.ProtectCluster {
			t.Fatalf("round %d: cluster limit exceeded by admitted jobs", round)
		}
	}
}

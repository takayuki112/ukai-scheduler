package tiers

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(n int) time.Time { return epoch.Add(time.Duration(n) * time.Minute) }

func policy() Policy {
	return Policy{Budget: UnitsOf(4), Allowance: UnitsOf(4), PromotedPriority: 75, ExcessPriority: 45, LargeJobs: LargeExcess}
}

func job(id string, c Class, units float64, running bool, n int) Job {
	prio := int32(75)
	if c == Promotable {
		prio = 50
	}
	return Job{ID: id, User: "a", Class: c, Units: UnitsOf(units), Priority: prio, Running: running, Created: at(n), Started: at(n)}
}
func p1(id string, u float64, run bool, n int) Job { return job(id, Budgeted, u, run, n) }
func p2(id string, u float64, run bool, n int) Job { return job(id, Promotable, u, run, n) }
func owned(o string, j Job) Job                    { j.User = o; return j }

func TestAssign(t *testing.T) {
	late := p2("late", 2, true, 1)
	late.Started = at(9)
	large := policy()
	large.LargeJobs = LargeNormalIfFirst
	for _, tc := range []struct {
		name   string
		policy Policy
		jobs   []Job
		want   map[string]Tier
	}{
		{"within budget", policy(), []Job{p1("x", 2, true, 1), p1("y", 2, false, 2)}, map[string]Tier{"x": Budget, "y": Budget}},
		{"over budget", policy(), []Job{p1("x", 4, true, 1), p1("y", 1, false, 2)}, map[string]Tier{"x": Budget, "y": Held}},
		{"small overtakes big", policy(), []Job{p1("x", 2, true, 1), p1("big", 4, false, 2), p1("small", 2, false, 3)}, map[string]Tier{"x": Budget, "big": Held, "small": Budget}},
		{"waiting jobs do not hold each other", policy(), []Job{p1("new", 4, false, 2), p1("old", 4, false, 1)}, map[string]Tier{"old": Budget, "new": Budget}},
		{"reserves oldest first, and only what fits", policy(), []Job{p1("new", 4, false, 2), p1("old", 1, false, 1), p2("a", 3, true, 3), p2("b", 1, true, 4)}, map[string]Tier{"old": Budget, "new": Budget, "a": Promoted, "b": Normal}},
		{"running over budget stays", policy(), []Job{p1("x", 8, true, 1), p2("y", 1, true, 2)}, map[string]Tier{"x": Budget, "y": Normal}},
		{"charged counts but is never held", policy(), []Job{job("w", Charged, 3, true, 1), job("w2", Charged, 9, false, 2), p1("x", 2, false, 3)}, map[string]Tier{"w": Budget, "w2": Budget, "x": Held}},
		{"users are separate", policy(), []Job{p1("x", 4, true, 1), p1("y", 4, false, 2), owned("b", p1("z", 4, false, 3))}, map[string]Tier{"x": Budget, "y": Held, "z": Budget}},
		{"promote, then normal, then excess", policy(), []Job{p2("a", 2, true, 1), p2("b", 2, true, 2), p2("c", 2, true, 3), p2("d", 2, true, 4), p2("e", 2, true, 5)}, map[string]Tier{"a": Promoted, "b": Promoted, "c": Normal, "d": Normal, "e": Excess}},
		{"starting a P1 demotes all", policy(), []Job{p1("x", 4, true, 9), p2("a", 2, true, 1), p2("b", 2, true, 2), p2("c", 2, true, 3), p2("d", 2, true, 4)}, map[string]Tier{"x": Budget, "a": Normal, "b": Normal, "c": Excess, "d": Excess}},
		{"P1 that fits demotes while pending", policy(), []Job{p1("x", 4, false, 9), p2("a", 2, true, 1)}, map[string]Tier{"x": Budget, "a": Normal}},
		{"P1 demotes only what it needs", policy(), []Job{p1("x", 2, false, 9), p2("a", 2, true, 1), p2("b", 2, true, 2)}, map[string]Tier{"x": Budget, "a": Promoted, "b": Normal}},
		{"P1 that does not fit keeps promotion", policy(), []Job{p1("x", 2, true, 1), p1("y", 4, false, 2), p2("a", 2, true, 3)}, map[string]Tier{"x": Budget, "y": Held, "a": Promoted}},
		{"impossible P1 keeps promotion", policy(), []Job{p1("x", 8, false, 1), p2("a", 4, true, 2)}, map[string]Tier{"x": Held, "a": Promoted}},
		{"promotion by start not creation", policy(), []Job{late, p2("b", 4, true, 5)}, map[string]Tier{"late": Normal, "b": Promoted}},
		{"allowance by creation not start", policy(), []Job{p1("x", 4, true, 1), late, p2("b", 4, true, 5)}, map[string]Tier{"x": Budget, "late": Normal, "b": Excess}},
		{"pending ignores promoted", policy(), []Job{p2("a", 4, true, 1), p2("b", 4, false, 2), p2("c", 1, false, 3)}, map[string]Tier{"a": Promoted, "b": Normal, "c": Excess}},
		{"pending counts running normal", policy(), []Job{p1("x", 4, true, 1), p2("a", 2, true, 2), p2("b", 4, false, 3)}, map[string]Tier{"x": Budget, "a": Normal, "b": Excess}},
		{"large is excess", policy(), []Job{p2("a", 8, true, 1), p2("b", 8, false, 2)}, map[string]Tier{"a": Excess, "b": Excess}},
		{"large is normal if first", large, []Job{p2("a", 8, true, 1), p2("b", 8, false, 2), p2("c", 1, true, 3)}, map[string]Tier{"a": Normal, "b": Excess, "c": Promoted}},
		{"fractions", policy(), []Job{p1("x", 3.5, true, 1), p1("y", 0.5, false, 2), p1("z", 0.6, false, 3)}, map[string]Tier{"x": Budget, "y": Budget, "z": Held}},
		{"no user", policy(), []Job{owned("", p1("x", 1, false, 1)), owned("", p1("y", 1, true, 1)), owned("", p2("z", 1, true, 1))}, map[string]Tier{"x": Held, "y": Budget, "z": Excess}},
		{"no GPUs or other class", policy(), []Job{p1("x", 0, false, 1), p2("y", 0, true, 1), owned("", p1("z", 0, false, 1)), job("o", Other, 9, true, 1)}, map[string]Tier{"x": Unmanaged, "y": Unmanaged, "z": Unmanaged, "o": Unmanaged}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Assign(tc.policy, tc.jobs)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d results, want %d", len(got), len(tc.want))
			}
			for id, want := range tc.want {
				if got[id].Tier != want {
					t.Errorf("%s: got %s, want %s", id, got[id].Tier, want)
				}
			}
			for _, j := range tc.jobs {
				r, want := got[j.ID], j.Priority
				switch r.Tier {
				case Promoted:
					want = 75
				case Excess:
					want = 45
				}
				if r.Priority != want {
					t.Errorf("%s: priority %d, want %d", j.ID, r.Priority, want)
				}
				if (r.Tier == Held) != (r.Reason != "") {
					t.Errorf("%s: tier %s has reason %q", j.ID, r.Tier, r.Reason)
				}
			}
		})
	}
}

func TestReasons(t *testing.T) {
	got := Assign(policy(), []Job{p1("x", 2.5, true, 1), p1("y", 2, false, 2), p1("z", 8, false, 3)})
	for id, want := range map[string]string{"y": "using 2.5 of 4", "z": "needs 8 units, budget is 4"} {
		if !strings.Contains(got[id].Reason, want) {
			t.Errorf("%s: %q lacks %q", id, got[id].Reason, want)
		}
	}
}

func TestNeverLowersByPromotionOrRaisesByExcess(t *testing.T) {
	hi, lo := p2("hi", 1, true, 1), p2("lo", 9, true, 2)
	hi.Priority, lo.Priority = 90, 30
	got := Assign(policy(), []Job{hi, lo})
	if got["hi"].Priority != 90 || got["lo"].Priority != 30 {
		t.Errorf("got %v", got)
	}
}

func TestUnits(t *testing.T) {
	for v, want := range map[float64]Units{0: 0, 1: 1000, 0.5: 500, 1.0 / 3: 334, 0.1 * 3: 300, 2.0001: 2001} {
		if got := UnitsOf(v); got != want {
			t.Errorf("UnitsOf(%v) = %d, want %d", v, got, want)
		}
	}
}

// Property tests.

func randomPolicy(r *rand.Rand) Policy {
	p := policy()
	p.Budget, p.Allowance = Units(r.Intn(9)*1000), Units(r.Intn(9)*1000)
	if r.Intn(2) == 0 {
		p.LargeJobs = LargeNormalIfFirst
	}
	return p
}

func randomJobs(r *rand.Rand) []Job {
	jobs := make([]Job, r.Intn(20))
	for i := range jobs {
		// Few distinct times, so ties are common.
		j := job(fmt.Sprintf("j%02d", r.Intn(1000)*100+i), Class(r.Intn(4)), float64(r.Intn(10))/2, r.Intn(2) == 0, r.Intn(8))
		j.Started = at(8 + r.Intn(8))
		j.User = []string{"", "a", "a", "b", "b", "c"}[r.Intn(6)]
		jobs[i] = j
	}
	return jobs
}

func property(t *testing.T, check func(t *testing.T, r *rand.Rand, p Policy, jobs []Job)) {
	t.Helper()
	for seed := int64(0); seed < 60000 && !t.Failed(); seed++ {
		r := rand.New(rand.NewSource(seed))
		p, jobs := randomPolicy(r), randomJobs(r)
		check(t, r, p, jobs)
		if t.Failed() {
			t.Logf("seed %d policy %+v", seed, p)
			for _, j := range jobs {
				t.Logf("%+v", j)
			}
		}
	}
}

func TestPropertyOrderIndependentAndPure(t *testing.T) {
	property(t, func(t *testing.T, r *rand.Rand, p Policy, jobs []Job) {
		before := append([]Job(nil), jobs...)
		got := Assign(p, jobs)
		if len(jobs) > 0 && !reflect.DeepEqual(before, jobs) {
			t.Error("modified its input")
		}
		r.Shuffle(len(before), func(i, k int) { before[i], before[k] = before[k], before[i] })
		if again := Assign(p, before); !reflect.DeepEqual(got, again) {
			t.Errorf("depends on order: %v vs %v", got, again)
		}
	})
}

func TestPropertyLimits(t *testing.T) {
	property(t, func(t *testing.T, _ *rand.Rand, p Policy, jobs []Job) {
		res := Assign(p, jobs)
		type sums struct{ used, promoted, normal, biggest Units }
		by := map[string]*sums{}
		for _, j := range jobs {
			s := by[j.User]
			if s == nil {
				s = &sums{}
				by[j.User] = s
			}
			if (j.Class == Budgeted || j.Class == Charged) && j.Running {
				s.used += j.Units
			}
			switch res[j.ID].Tier {
			case Promoted:
				s.promoted += j.Units
				if !j.Running || j.User == "" {
					t.Errorf("%s promoted while pending or unowned", j.ID)
				}
			case Normal:
				s.normal += j.Units
				s.biggest = max(s.biggest, j.Units)
			case Held:
				if j.Running || j.Class != Budgeted {
					t.Errorf("%s held", j.ID)
				}
			}
		}
		for _, j := range jobs {
			if j.Class == Budgeted && !j.Running && j.User != "" && j.Units > 0 {
				if held := by[j.User].used+j.Units > p.Budget; held != (res[j.ID].Tier == Held) {
					t.Errorf("%s: held should be %v", j.ID, held)
				}
			}
		}
		for o, s := range by {
			if s.promoted > 0 && s.used+s.promoted > p.Budget {
				t.Errorf("%s: promotion exceeds budget", o)
			}
			alone := p.LargeJobs == LargeNormalIfFirst && s.normal == s.biggest
			if s.normal > p.Allowance && !alone {
				t.Errorf("%s: allowance exceeded", o)
			}
		}
	})
}

func TestPropertyUsersAreIndependent(t *testing.T) {
	property(t, func(t *testing.T, _ *rand.Rand, p Policy, jobs []Job) {
		all := Assign(p, jobs)
		var mine []Job
		for _, j := range jobs {
			if j.User == "a" {
				mine = append(mine, j)
			}
		}
		for id, r := range Assign(p, mine) {
			if all[id] != r {
				t.Errorf("%s depends on other users", id)
			}
		}
	})
}

// Simulates a session in which promotable jobs preempt lower-priority ones, and
// checks that no victim could evict its preemptor in the next session. With
// delayStarts, preemptors start only after other jobs that started at once, as
// when they wait for a victim to terminate.
func simulateSwap(t *testing.T, delayStarts bool) (preemptions int) {
	property(t, func(t *testing.T, r *rand.Rand, p Policy, jobs []Job) {
		for i := range jobs {
			jobs[i].User = []string{"a", "b", "c"}[r.Intn(3)]
			if r.Intn(4) != 0 { // mostly promotable, or few jobs could preempt
				jobs[i].Class, jobs[i].Priority = Promotable, 50
			}
		}
		res := Assign(p, jobs)
		next := append([]Job(nil), jobs...)
		type pair struct{ preemptor, victim string }
		var pairs []pair
		touched := map[string]bool{}
		for _, i := range r.Perm(len(next)) {
			a := &next[i]
			if a.Class != Promotable || a.Running || touched[a.ID] || r.Intn(3) == 0 {
				continue
			}
			var victims []*Job
			if r.Intn(4) != 0 {
				for _, k := range r.Perm(len(next)) {
					b := &next[k]
					if b.Class == Promotable && b.Running && !touched[b.ID] && res[b.ID].Priority < res[a.ID].Priority && len(victims) < 2 {
						victims = append(victims, b)
					}
				}
				if len(victims) == 0 {
					continue
				}
			} // else it takes free GPUs
			a.Running, a.Started = true, at(100)
			if delayStarts && len(victims) > 0 {
				a.Started = at(101)
			}
			touched[a.ID] = true
			for _, b := range victims {
				b.Running = false
				touched[b.ID] = true
				pairs = append(pairs, pair{a.ID, b.ID})
			}
		}
		after := Assign(p, next)
		preemptions += len(pairs)
		for _, pr := range pairs {
			if after[pr.victim].Priority > after[pr.preemptor].Priority {
				t.Errorf("%s evicted %s, which can evict it back (%d > %d)", pr.preemptor, pr.victim, after[pr.victim].Priority, after[pr.preemptor].Priority)
			}
		}
	})
	return preemptions
}

func TestPropertyNoSwap(t *testing.T) {
	for _, delay := range []bool{false, true} {
		t.Run(fmt.Sprintf("delayStarts=%v", delay), func(t *testing.T) {
			if n := simulateSwap(t, delay); n < 10000 {
				t.Errorf("only %d preemptions simulated", n)
			} else {
				t.Logf("%d preemptions", n)
			}
		})
	}
}

func BenchmarkAssign(b *testing.B) {
	for _, n := range []int{100, 1300, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			r := rand.New(rand.NewSource(1))
			jobs := make([]Job, n)
			for i := range jobs {
				jobs[i] = job(fmt.Sprint(i), Class(1+r.Intn(3)), float64(1+r.Intn(4)), r.Intn(2) == 0, r.Intn(10000))
				jobs[i].User = fmt.Sprint(r.Intn(50))
			}
			b.ReportAllocs()
			for b.Loop() {
				Assign(policy(), jobs)
			}
		})
	}
}

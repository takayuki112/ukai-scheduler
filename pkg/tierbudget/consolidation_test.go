package tierbudget

import (
	"fmt"
	"testing"

	enginev2alpha2 "github.com/kai-scheduler/KAI-scheduler/pkg/apis/scheduling/v2alpha2"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/allocate"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/consolidation"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_status"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/podgroup_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"
)

func on(node string, t testJob) testJob { t.node = node; return t }

// fragmented has 2 of 4 GPUs free on each node, so a job of 4 GPUs fits only if a
// or b moves to the other's node.
func fragmented(classA, classB string, waiting ...testJob) []testJob {
	return append([]testJob{
		on("n0", j("a", "alice", classA, 2, running, 90)),
		on("n1", j("b", "bob", classB, 2, running, 90)),
	}, waiting...)
}

const p1ForP1 = "high>high,mid,low;mid,low>mid,low"

func TestConsolidation(t *testing.T) {
	low := j("w", "carol", "low", 4, pending, 5)
	high := j("w", "carol", "high", 4, pending, 5)
	for _, tc := range []struct {
		name, passes, budget string
		jobs                 []testJob
		placed               bool
		still                []string // jobs that must keep running
	}{
		{"default: P3 does not move P1", "", "4", fragmented("high", "high", low), false, []string{"a", "b"}},
		{"default: P1 does not move P1", "", "4", fragmented("high", "high", high), false, []string{"a", "b"}},
		{"default: P3 does not move promoted", "", "2", fragmented("mid", "mid", low), false, []string{"a", "b"}},
		{"default: P3 does not move workspaces", "", "4", fragmented("workspace", "workspace", low), false, []string{"a", "b"}},
		{"default: P3 does not move P0", "", "4", fragmented("top", "top", low), false, []string{"a", "b"}},
		{"default: P3 moves P3 and not P1", "", "4", fragmented("high", "low", low), true, []string{"a"}},
		{"default: P1 moves P3", "", "4", fragmented("low", "low", high), true, nil},
		{"default: P3 moves P2 that is not promoted", "", "0", fragmented("mid", "mid", low), true, nil},
		{"default: a held P1 moves nothing", "", "2", fragmented("low", "low", high), false, []string{"a", "b"}},
		{"P1 moves P1 if allowed", p1ForP1, "4", fragmented("high", "high", high), true, nil},
		{"P3 does not move P1 even then", p1ForP1, "4", fragmented("high", "high", low), false, []string{"a", "b"}},
		{"P1 moves P3", p1ForP1, "4", fragmented("high", "low", high), true, nil},
		{"P3 moves P3", p1ForP1, "4", fragmented("low", "low", low), true, nil},
		{"promoted is not in its class", p1ForP1, "2", fragmented("mid", "mid", low), false, []string{"a", "b"}},
		{"promoted can be allowed", "low>@promoted", "2", fragmented("mid", "mid", low), true, nil},
		{"unlisted is not placed", "high>low", "4", fragmented("low", "low", low), false, []string{"a", "b"}},
		{"unlisted is not moved", "low>mid", "4", fragmented("low", "low", low), false, []string{"a", "b"}},
		{"placed in the second pass", "high>low;low>low", "4", fragmented("low", "low", low), true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ssn := session(t, map[string]int{"n0": 4, "n1": 4}, tc.jobs)
			args := testArgs(tc.budget)
			args["consolidation"] = tc.passes
			if tc.budget == "0" {
				args["allowance"] = "0"
			}
			original := preemptibility(ssn)
			// The marks must not be visible in any accounting.
			ssn.AddEventHandler(&framework.EventHandler{
				AllocateFunc:   func(e *framework.Event) { checkUnmarked(t, ssn, e, original) },
				DeallocateFunc: func(e *framework.Event) { checkUnmarked(t, ssn, e, original) },
			})
			p := open(ssn, args)
			allocate.New().Execute(ssn)
			NewConsolidation().Execute(ssn)

			if got := state(find(t, ssn, "w")) == "placed"; got != tc.placed {
				t.Errorf("w placed=%v, want %v", got, tc.placed)
			}
			for _, name := range tc.still {
				if got := state(find(t, ssn, name)); got != "running" {
					t.Errorf("%s is %s", name, got)
				}
			}
			checkRestored(t, ssn, original)
			p.OnSessionClose(ssn)
			if openSession.Load() != nil {
				t.Error("session is kept after it closed")
			}
		})
	}
}

// Without an enforcing plugin the action must do what KAI's does.
func TestConsolidationIsStockOtherwise(t *testing.T) {
	audit := testArgs("4")
	audit["mode"] = "audit"
	jobs := fragmented("high", "high", j("w", "carol", "low", 4, pending, 5))
	nodes := map[string]int{"n0": 4, "n1": 4}
	for name, prepare := range map[string]func(*framework.Session){
		"no plugin": func(*framework.Session) {},
		"audit":     func(ssn *framework.Session) { open(ssn, audit) },
		"invalid":   func(ssn *framework.Session) { open(ssn, framework.PluginArguments{}) },
		"other session": func(*framework.Session) {
			open(session(t, nodes, jobs), testArgs("4"))
		},
	} {
		for _, action := range []framework.Action{NewConsolidation(), consolidation.New()} {
			t.Run(name+"/"+string(action.Name()), func(t *testing.T) {
				openSession.Store(nil)
				ssn := session(t, nodes, jobs)
				prepare(ssn)
				allocate.New().Execute(ssn)
				action.Execute(ssn)
				if got := state(find(t, ssn, "w")); got != "placed" {
					t.Errorf("w is %s", got)
				}
			})
		}
	}
}

type panics struct{ framework.Action }

func (panics) Execute(*framework.Session) { panic("stock") }

func TestConsolidationRestoresAfterPanic(t *testing.T) {
	ssn := session(t, map[string]int{"n0": 4, "n1": 4}, fragmented("high", "low", j("w", "carol", "low", 4, pending, 5)))
	original := preemptibility(ssn)
	open(ssn, testArgs("4"))
	defer func() {
		if recover() == nil {
			t.Error("KAI's own panic was hidden")
		}
		checkRestored(t, ssn, original)
	}()
	(&consolidationAction{stock: panics{}}).Execute(ssn)
}

// A partly running job is protected as a whole.
func TestConsolidationProtectsPartlyRunning(t *testing.T) {
	ssn := session(t, map[string]int{"n0": 4}, []testJob{{name: "a", owner: "alice", class: "high", gpus: 1, pods: []pod_status.PodStatus{running, pending}, age: 9}})
	p := open(ssn, testArgs("4"))
	a := find(t, ssn, "a")
	if p.allowedByDefault(a) || p.allowedIn(Pass{Place: []string{"high"}, Move: []string{"low"}}, a) {
		t.Error("not protected")
	}
}

func preemptibility(ssn *framework.Session) map[*podgroup_info.PodGroupInfo]enginev2alpha2.Preemptibility {
	m := map[*podgroup_info.PodGroupInfo]enginev2alpha2.Preemptibility{}
	for _, info := range ssn.ClusterInfo.PodGroupInfos {
		m[info] = info.Preemptibility
	}
	return m
}

func checkRestored(t *testing.T, ssn *framework.Session, original map[*podgroup_info.PodGroupInfo]enginev2alpha2.Preemptibility) {
	t.Helper()
	for _, info := range ssn.ClusterInfo.PodGroupInfos {
		if info.Preemptibility != original[info] {
			t.Errorf("%s left %s", info.Name, info.Preemptibility)
		}
	}
}

func checkUnmarked(t *testing.T, ssn *framework.Session, e *framework.Event, original map[*podgroup_info.PodGroupInfo]enginev2alpha2.Preemptibility) {
	t.Helper()
	if info := ssn.ClusterInfo.PodGroupInfos[e.Task.Job]; info.Preemptibility != original[info] {
		t.Errorf("event for %s while marked", info.Name)
	}
}

func TestWarnsAboutStockConsolidation(t *testing.T) {
	for actions, warns := range map[string]bool{"allocate, consolidation, preempt": true, "allocate, ukai-consolidation": false} {
		errorLog = &changeLog{}
		ssn := session(t, map[string]int{"n0": 4}, nil)
		ssn.Config.Actions = actions
		open(ssn, testArgs("4"))
		if got := errorLog.last["consolidation"] != ""; got != warns {
			t.Errorf("%s: warned=%v", actions, got)
		}
	}
}

// Nothing can be placed, so every pass searches in full.
func BenchmarkConsolidation(b *testing.B) {
	nodes := map[string]int{}
	var jobs []testJob
	for n := range 20 {
		node := fmt.Sprint("n", n)
		nodes[node] = 8
		for k := range 3 {
			jobs = append(jobs, on(node, j(fmt.Sprint("r", n, "-", k), fmt.Sprint("o", n), []string{"mid", "low", "low"}[k], 2, running, 90)))
		}
	}
	for k := range 10 {
		jobs = append(jobs, j(fmt.Sprint("w", k), fmt.Sprint("o", k), []string{"high", "low"}[k%2], 8, pending, 5))
	}
	for _, tc := range []struct{ name, passes string }{
		{"stock", "-"}, {"default", ""}, {"two passes", "high>high,mid,low;mid,low>mid,low"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				ssn := session(&testing.T{}, nodes, jobs)
				action := NewConsolidation()
				openSession.Store(nil)
				if tc.passes != "-" {
					args := testArgs("8")
					args["allowance"], args["consolidation"] = "0", tc.passes
					open(ssn, args)
				}
				allocate.New().Execute(ssn)
				b.StartTimer()
				action.Execute(ssn)
			}
		})
	}
}

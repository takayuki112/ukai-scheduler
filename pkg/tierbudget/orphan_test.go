package tierbudget

import (
	"strings"
	"testing"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_status"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// drop deletes a job's PodGroup and leaves its pods on their nodes.
func drop(t *testing.T, ssn *framework.Session, name, class, label string) {
	t.Helper()
	info := find(t, ssn, name)
	for _, task := range info.GetAllPodsMap() {
		task.Job = info.UID
		task.Pod.Spec.PriorityClassName = class
		task.Pod.Labels = map[string]string{}
		if label != "" {
			task.Pod.Labels[ownerLabel] = label
		}
	}
	delete(ssn.ClusterInfo.PodGroupInfos, info.UID)
}

func TestOrphans(t *testing.T) {
	jobs := func(class string, status pod_status.PodStatus) []testJob {
		return []testJob{
			{name: "gone", owner: "alice", class: class, gpus: 1, pods: []pod_status.PodStatus{status, status}, age: 9},
			j("a", "alice", "high", 1, pending, 8), j("b", "bob", "high", 1, pending, 7),
		}
	}
	nodes := map[string]int{"n0": 8}
	for _, tc := range []struct {
		name, class string
		status      pod_status.PodStatus
		seen        bool // by an earlier session, with its PodGroup
		label       string
		held        bool
	}{
		{"known, terminating", "high", releasing, true, "", true},
		{"known, running", "high", running, true, "", true},
		{"known, and the label lies", "high", releasing, true, "bob", true},
		{"after a restart, by label", "high", releasing, false, "alice", true},
		{"after a restart, without a label", "high", releasing, false, "", false},
		{"of another owner", "high", releasing, false, "carol", false},
		{"known, and the pod's class lies", "low/high", releasing, true, "", false},
		{"known, and the pod's class lies the other way", "high/low", releasing, true, "", true},
		{"charged", "workspace", releasing, true, "", true},
		{"promotable", "mid", releasing, true, "", false},
		{"of another class", "top", releasing, true, "alice", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// PodGroup's class / pod's class
			class, podClass, ok := strings.Cut(tc.class, "/")
			if !ok {
				podClass = class
			}
			tc.class = class
			lastGroups.Store(nil)
			if tc.seen {
				open(session(t, nodes, jobs(tc.class, tc.status)), testArgs("2"))
			}
			// It stays known for as long as it is there.
			for range 2 {
				ssn := session(t, nodes, jobs(tc.class, tc.status))
				drop(t, ssn, "gone", podClass, tc.label)
				p := schedule(ssn, testArgs("2"))
				want := "placed"
				if tc.held {
					want = "pending"
				}
				if got := state(find(t, ssn, "a")); got != want {
					t.Errorf("a: got %s, want %s", got, want)
				}
				if got := state(find(t, ssn, "b")); got != "placed" {
					t.Errorf("b: %s", got)
				}
				if tc.held {
					var found bool
					for _, s := range LastStatus().Jobs {
						if s.Orphan {
							found = s.Owner == "alice" && s.Running && s.Units == 2 && s.Tier == tiers.Budget
						}
						if s.Name == "a" && !strings.Contains(s.Reason, "using 2 of 2") {
							t.Errorf("reason %q", s.Reason)
						}
					}
					if !found {
						t.Errorf("status: %+v", LastStatus().Jobs)
					}
				}
				p.OnSessionClose(ssn)
			}
			// Then it is forgotten.
			open(session(t, nodes, jobs(tc.class, tc.status)[1:]), testArgs("2"))
			if n := len(*lastGroups.Load()); n != 2 {
				t.Errorf("%d known, want 2", n)
			}
		})
	}
}

// A pod of no PodGroup, for example of another scheduler, is not an orphan.
func TestPodsOfOtherSchedulersAreNotOrphans(t *testing.T) {
	ssn := session(t, map[string]int{"n0": 8}, []testJob{j("x", "alice", "high", 2, running, 9), j("a", "alice", "high", 1, pending, 8)})
	drop(t, ssn, "x", "high", "alice")
	for _, task := range ssn.ClusterInfo.Nodes["n0"].PodInfos {
		task.Job = ""
	}
	schedule(ssn, testArgs("2"))
	if got := state(find(t, ssn, "a")); got != "placed" {
		t.Errorf("a: %s", got)
	}
}

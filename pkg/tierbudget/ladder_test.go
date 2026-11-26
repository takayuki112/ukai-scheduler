package tierbudget

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_status"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"

	"github.com/takayuki112/ukai-scheduler/pkg/ladder"
)

func ladderArgs(extra map[string]string) framework.PluginArguments {
	args := framework.PluginArguments{
		"mode": "enforce", "userLabel": userLabel, "scheme": "ladder", "ladderClasses": "mid",
		"ladder": "75=4, 70=4, 40", "levelLabel": "example.com/level", "protectLabel": "example.com/protect",
		"whenFullLabel": "example.com/when-full", "protectLimit": "12",
	}
	maps.Copy(args, extra)
	return args
}

func resetActiveUsers() {
	activeUsers.Lock()
	activeUsers.seen = map[string]time.Time{}
	activeUsers.Unlock()
}

func label(t *testing.T, ssn *framework.Session, name, key, value string) {
	t.Helper()
	find(t, ssn, name).PodGroup.Labels[key] = value
}

func TestLadderPriorities(t *testing.T) {
	resetActiveUsers()
	ssn := session(t, map[string]int{"n0": 8, "n1": 8}, []testJob{
		{name: "a", user: "alice", class: "mid", gpus: 2, pods: []pod_status.PodStatus{running}, age: 40},
		{name: "b", user: "alice", class: "mid", gpus: 2, pods: []pod_status.PodStatus{running}, age: 30},
		{name: "c", user: "alice", class: "mid", gpus: 2, pods: []pod_status.PodStatus{running}, age: 20},
		{name: "d", user: "alice", class: "mid", gpus: 4, pods: []pod_status.PodStatus{running}, age: 10, node: "n1"},
		{name: "x", user: "bob", class: "mid", gpus: 2, pods: []pod_status.PodStatus{running}, age: 5, node: "n1"},
		{name: "t", user: "carol", class: "top", gpus: 1, pods: []pod_status.PodStatus{pending}, age: 1},
	})
	open(ssn, ladderArgs(nil))
	for name, want := range map[string]int32{"a": 75, "b": 75, "c": 70, "d": 40, "x": 75, "t": 90} {
		if got := find(t, ssn, name).Priority; got != want {
			t.Errorf("%s: priority %d, want %d", name, got, want)
		}
	}
	status := LastStatus()
	if status.Scheme != SchemeLadder {
		t.Errorf("scheme %q", status.Scheme)
	}
}

func TestLadderEvictionGap(t *testing.T) {
	for _, tc := range []struct {
		gap     string
		evicted bool
	}{{"", true}, {"5", true}, {"10", false}} {
		t.Run("gap "+tc.gap, func(t *testing.T) {
			resetActiveUsers()
			ssn := session(t, map[string]int{"n0": 4, "n1": 4}, []testJob{
				{name: "a", user: "alice", class: "mid", gpus: 4, pods: []pod_status.PodStatus{running}, age: 40},
				{name: "b", user: "alice", class: "mid", gpus: 4, pods: []pod_status.PodStatus{running}, age: 30, node: "n1"},
				{name: "x", user: "bob", class: "mid", gpus: 4, pods: []pod_status.PodStatus{pending}, age: 1},
			})
			args := ladderArgs(nil)
			if tc.gap != "" {
				args["evictGap"] = tc.gap
			}
			schedule(ssn, args)
			// b is at 70, x would be at 75.
			if got := state(find(t, ssn, "b")) == "evicted"; got != tc.evicted {
				t.Errorf("b evicted: %v, want %v", got, tc.evicted)
			}
			if got := state(find(t, ssn, "a")); got != "running" {
				t.Errorf("a is %s", got)
			}
		})
	}
}

func TestLadderFloorIsEvictedAcrossTheGap(t *testing.T) {
	resetActiveUsers()
	ssn := session(t, map[string]int{"n0": 4, "n1": 4, "n2": 4}, []testJob{
		{name: "a", user: "alice", class: "mid", gpus: 4, pods: []pod_status.PodStatus{running}, age: 40},
		{name: "b", user: "alice", class: "mid", gpus: 4, pods: []pod_status.PodStatus{running}, age: 30, node: "n1"},
		{name: "c", user: "alice", class: "mid", gpus: 4, pods: []pod_status.PodStatus{running}, age: 20, node: "n2"},
		{name: "x", user: "bob", class: "mid", gpus: 4, pods: []pod_status.PodStatus{pending}, age: 1},
	})
	schedule(ssn, ladderArgs(map[string]string{"evictGap": "10"}))
	for name, want := range map[string]string{"a": "running", "b": "running", "c": "evicted", "x": "placed"} {
		if got := state(find(t, ssn, name)); got != want {
			t.Errorf("%s is %s, want %s", name, got, want)
		}
	}
}

func TestLadderProtectedIsNotEvicted(t *testing.T) {
	for _, protect := range []bool{false, true} {
		resetActiveUsers()
		ssn := session(t, map[string]int{"n0": 4}, []testJob{
			{name: "p", user: "alice", class: "mid", gpus: 4, pods: []pod_status.PodStatus{running}, age: 40},
			{name: "t", user: "carol", class: "top", gpus: 4, pods: []pod_status.PodStatus{pending}, age: 1},
		})
		if protect {
			label(t, ssn, "p", "example.com/protect", "true")
		}
		schedule(ssn, ladderArgs(nil))
		if got := state(find(t, ssn, "p")) == "evicted"; got == protect {
			t.Errorf("protected %v: evicted %v", protect, got)
		}
	}
}

func TestLadderProtectionEnds(t *testing.T) {
	resetActiveUsers()
	ssn := session(t, map[string]int{"n0": 4}, []testJob{
		{name: "p", user: "alice", class: "mid", gpus: 4, pods: []pod_status.PodStatus{running}, age: 40},
		{name: "t", user: "carol", class: "top", gpus: 4, pods: []pod_status.PodStatus{pending}, age: 1},
	})
	label(t, ssn, "p", "example.com/protect", "true")
	schedule(ssn, ladderArgs(map[string]string{"protectFor": "30m"}))
	if got := state(find(t, ssn, "p")); got != "evicted" {
		t.Errorf("p is %s", got)
	}
}

func TestLadderLevelHeld(t *testing.T) {
	resetActiveUsers()
	ssn := session(t, map[string]int{"n0": 8}, []testJob{
		{name: "a", user: "alice", class: "mid", gpus: 4, pods: []pod_status.PodStatus{running}, age: 40},
		{name: "b", user: "alice", class: "mid", gpus: 2, pods: []pod_status.PodStatus{pending}, age: 1},
		{name: "c", user: "alice", class: "mid", gpus: 2, pods: []pod_status.PodStatus{pending}, age: 1},
	})
	label(t, ssn, "a", "example.com/level", "75")
	label(t, ssn, "b", "example.com/level", "75")
	label(t, ssn, "c", "example.com/level", "75")
	label(t, ssn, "c", "example.com/when-full", "lower")
	p := schedule(ssn, ladderArgs(nil))
	if got := state(find(t, ssn, "b")); got != "pending" {
		t.Errorf("b is %s", got)
	}
	if got := state(find(t, ssn, "c")); got != "placed" {
		t.Errorf("c is %s", got)
	}
	var task *pod_info.PodInfo
	for _, task = range find(t, ssn, "b").GetAllPodsMap() {
	}
	if reason := p.hold(task); !strings.HasPrefix(reason, "level 75: needs 2 units, using 4 of 4") {
		t.Errorf("reason %q", reason)
	}
	if got := find(t, ssn, "c").Priority; got != 70 {
		t.Errorf("c priority %d", got)
	}
}

func TestLadderShare(t *testing.T) {
	resetActiveUsers()
	ssn := session(t, map[string]int{"n0": 8}, []testJob{
		{name: "a", user: "alice", class: "mid", gpus: 2, pods: []pod_status.PodStatus{running}, age: 40},
		{name: "b", user: "alice", class: "mid", gpus: 2, pods: []pod_status.PodStatus{running}, age: 30},
		{name: "x", user: "bob", class: "mid", gpus: 1, pods: []pod_status.PodStatus{running}, age: 30},
	})
	open(ssn, ladderArgs(map[string]string{"ladderWidth": "share", "ladder": "75=0.5, 70=0.5, 40", "protectLimit": "1"}))
	// 8 GPUs, 2 users: a share of 4, so steps of 2.
	if s := LastStatus().Share; s != 4 {
		t.Errorf("share %v", s)
	}
	for name, want := range map[string]int32{"a": 75, "b": 70, "x": 75} {
		if got := find(t, ssn, name).Priority; got != want {
			t.Errorf("%s: priority %d, want %d", name, got, want)
		}
	}
}

func TestActiveUsersWindow(t *testing.T) {
	resetActiveUsers()
	now := time.Now()
	jobs := []ladder.Job{{User: "alice", Units: 1}, {User: "bob", Units: 1}}
	if n := countActive(jobs, now, time.Hour); n != 2 {
		t.Errorf("%d", n)
	}
	if n := countActive(jobs[:1], now.Add(30*time.Minute), time.Hour); n != 2 {
		t.Errorf("bob should still count: %d", n)
	}
	if n := countActive(jobs[:1], now.Add(90*time.Minute), time.Hour); n != 1 {
		t.Errorf("bob should be gone: %d", n)
	}
}

func TestLadderConfigErrors(t *testing.T) {
	for args, want := range map[string]string{
		"ladder=75=4,80":        "priorities must fall",
		"ladder=75,40":          "needs a width",
		"ladder=75=4,40=2":      "needs a width",
		"budget=4":              "not with scheme=ladder",
		"protectCost=0.5":       "at least 1",
		"protectClusterLimit=2": "a fraction",
		"protectFor=soon":       "not a duration",
		"ladderWidth=gpus":      "is not one of",
	} {
		key, value, _ := strings.Cut(args, "=")
		_, err := ParseConfig(ladderArgs(map[string]string{key: value}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", args, err, want)
		}
	}
	if _, err := ParseConfig(framework.PluginArguments{"userLabel": "u", "budgetedClasses": "a", "budget": "1", "ladder": "75"}); err == nil ||
		!strings.Contains(err.Error(), "only with scheme=ladder") {
		t.Errorf("ladder argument with the budget: %v", err)
	}
}

func TestLadderEvictsTheLastStarted(t *testing.T) {
	resetActiveUsers()
	// a was created first but restarted recently, so it loses the least. KAI alone
	// would evict b, the one created last.
	ssn := session(t, map[string]int{"n0": 4, "n1": 4}, []testJob{
		{name: "a", user: "alice", class: "mid", gpus: 4, pods: []pod_status.PodStatus{running}, age: 100, started: 5},
		{name: "b", user: "bob", class: "mid", gpus: 4, pods: []pod_status.PodStatus{running}, age: 50, started: 40, node: "n1"},
		{name: "t", user: "carol", class: "top", gpus: 4, pods: []pod_status.PodStatus{pending}, age: 1},
	})
	schedule(ssn, ladderArgs(nil))
	for name, want := range map[string]string{"a": "evicted", "b": "running"} {
		if got := state(find(t, ssn, name)); got != want {
			t.Errorf("%s is %s, want %s", name, got, want)
		}
	}
}

package tierbudget

import (
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/allocate"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/consolidation"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/preempt"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/common_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_status"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/podgroup_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/resource_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/test_utils"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/test_utils/jobs_fake"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/test_utils/nodes_fake"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/test_utils/tasks_fake"
)

const ownerLabel = "example.com/owner"

const (
	pending   = pod_status.Pending
	running   = pod_status.Running
	releasing = pod_status.Releasing
)

var classPriority = map[string]int32{"top": 90, "high": 75, "workspace": 75, "mid": 50, "low": 25}

func testArgs(budget string) framework.PluginArguments {
	return framework.PluginArguments{
		"mode": "enforce", "ownerLabel": ownerLabel, "budget": budget, "allowance": "4",
		"budgetedClasses": "high", "chargedClasses": "workspace", "promotableClasses": "mid",
		"promotedPriority": "75", "excessPriority": "45",
	}
}

type testJob struct {
	name, owner, class string
	gpus               float64 // per pod
	pods               []pod_status.PodStatus
	node               string   // for pods that are not pending; default n0
	nodes              []string // if set, each pod\'s node
	age                int      // minutes since creation
	started            int      // minutes since start, if running; default age
}

// j is a job of one pod.
func j(name, owner, class string, gpus float64, status pod_status.PodStatus, age int) testJob {
	return testJob{name: name, owner: owner, class: class, gpus: gpus, pods: []pod_status.PodStatus{status}, age: age}
}

func session(t *testing.T, nodes map[string]int, jobs []testJob) *framework.Session {
	t.Helper()
	test_utils.InitTestingInfrastructure()
	topology := test_utils.TestTopologyBasic{
		Name:   t.Name(),
		Nodes:  map[string]nodes_fake.TestNodeBasic{},
		Queues: []test_utils.TestQueueBasic{{Name: "q", DeservedGPUs: 100, GPUOverQuotaWeight: 1}},
		Mocks: &test_utils.TestMock{CacheRequirements: &test_utils.CacheMocking{
			NumberOfCacheBinds: 99, NumberOfCacheEvictions: 99, NumberOfPipelineActions: 99,
		}},
	}
	for name, gpus := range nodes {
		topology.Nodes[name] = nodes_fake.TestNodeBasic{GPUs: gpus}
	}
	for _, tj := range jobs {
		job := &jobs_fake.TestJobBasic{
			Name: tj.name, QueueName: "q", RequiredGPUsPerTask: tj.gpus,
			Priority: classPriority[tj.class], JobAgeInMinutes: tj.age,
		}
		for i, status := range tj.pods {
			task := &tasks_fake.TestTaskBasic{State: status}
			if status != pending {
				task.NodeName = tj.node
				if tj.node == "" {
					task.NodeName = "n0"
				}
				if tj.nodes != nil {
					task.NodeName = tj.nodes[i]
				}
			}
			job.Tasks = append(job.Tasks, task)
		}
		topology.Jobs = append(topology.Jobs, job)
	}
	ssn := test_utils.BuildSession(topology, gomock.NewController(t))
	// As in production: skip jobs that look like one that has already failed.
	ssn.SchedulerParams.UseSchedulingSignatures = true
	for _, tj := range jobs {
		info := find(t, ssn, tj.name)
		info.PodGroup.Spec.PriorityClassName = tj.class
		for _, task := range info.GetAllPodsMap() {
			task.Job = info.UID
			task.Pod.Spec.PriorityClassName = tj.class
		}
		if tj.owner != "" {
			info.PodGroup.Labels = map[string]string{ownerLabel: tj.owner}
		}
		if info.LastStartTimestamp != nil {
			minutes := tj.age
			if tj.started != 0 {
				minutes = tj.started
			}
			started := time.Now().Add(-time.Duration(minutes) * time.Minute)
			info.LastStartTimestamp = &started
		}
	}
	return ssn
}

func find(t *testing.T, ssn *framework.Session, name string) *podgroup_info.PodGroupInfo {
	t.Helper()
	for _, info := range ssn.ClusterInfo.PodGroupInfos {
		if info.Name == name {
			return info
		}
	}
	t.Fatalf("no job %s", name)
	return nil
}

func open(ssn *framework.Session, args framework.PluginArguments) *plugin {
	p := New(args).(*plugin)
	p.OnSessionOpen(ssn)
	return p
}

// schedule opens the plugin and runs KAI's actions in their default order.
func schedule(ssn *framework.Session, args framework.PluginArguments) *plugin {
	p := open(ssn, args)
	allocate.New().Execute(ssn)
	consolidation.New().Execute(ssn)
	preempt.New().Execute(ssn)
	return p
}

// state summarises a job: pending, placed (also if waiting for a victim to leave),
// evicted or running.
func state(info *podgroup_info.PodGroupInfo) string {
	var s []string
	for _, task := range info.GetAllPodsMap() {
		switch {
		case task.Status == pod_status.Releasing:
			s = append(s, "evicted")
		case task.Status == pod_status.Running:
			s = append(s, "running")
		case pod_status.IsActiveAllocatedStatus(task.Status):
			s = append(s, "placed")
		default:
			s = append(s, "pending")
		}
	}
	for _, x := range s {
		if x != s[0] {
			return "mixed"
		}
	}
	return s[0]
}

func TestScheduling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget string
		nodes  map[string]int
		jobs   []testJob
		want   map[string]string
		stock  map[string]string // what differs without the plugin, to prove the case tests it
	}{
		{
			"over budget is held although GPUs are free", "2", map[string]int{"n0": 8},
			[]testJob{j("a1", "alice", "high", 2, running, 9), j("a2", "alice", "high", 1, pending, 5), j("b1", "bob", "high", 2, pending, 4)},
			map[string]string{"a1": "running", "a2": "pending", "b1": "placed"},
			map[string]string{"a2": "placed"},
		},
		{
			"only as many as fit are placed in one session", "2", map[string]int{"n0": 8},
			[]testJob{j("a1", "alice", "high", 1, pending, 9), j("a2", "alice", "high", 1, pending, 8), j("a3", "alice", "high", 1, pending, 7)},
			map[string]string{"a1": "placed", "a2": "placed", "a3": "pending"},
			map[string]string{"a3": "placed"},
		},
		{
			"small overtakes one that does not fit the budget", "4", map[string]int{"n0": 8},
			[]testJob{j("a1", "alice", "high", 2, running, 9), j("big", "alice", "high", 4, pending, 8), j("small", "alice", "high", 2, pending, 7)},
			map[string]string{"big": "pending", "small": "placed"},
			map[string]string{"big": "placed"},
		},
		{
			"small overtakes one that does not fit the GPUs", "8", map[string]int{"n0": 4},
			[]testJob{j("big", "alice", "high", 8, pending, 8), j("small", "alice", "high", 2, pending, 7)},
			map[string]string{"big": "pending", "small": "placed"}, nil,
		},
		{
			"a held job does not block another owner's preemption", "1", map[string]int{"n0": 2},
			[]testJob{j("a1", "alice", "high", 1, running, 9), j("victim", "carol", "low", 1, running, 9), j("a2", "alice", "high", 1, pending, 8), j("b1", "bob", "high", 1, pending, 7)},
			map[string]string{"a1": "running", "a2": "pending", "b1": "placed", "victim": "evicted"},
			map[string]string{"a2": "placed", "b1": "pending"},
		},
		{
			"workspaces count but are not held", "2", map[string]int{"n0": 8},
			[]testJob{j("w1", "alice", "workspace", 2, running, 9), j("w2", "alice", "workspace", 4, pending, 8), j("a1", "alice", "high", 1, pending, 7)},
			map[string]string{"w2": "placed", "a1": "pending"},
			map[string]string{"a1": "placed"},
		},
		{
			"no owner is held", "2", map[string]int{"n0": 8},
			[]testJob{j("x", "", "high", 1, pending, 9)},
			map[string]string{"x": "pending"},
			map[string]string{"x": "placed"},
		},
		{
			"promoted is safe from another owner's P1", "2", map[string]int{"n0": 2},
			[]testJob{j("p2", "alice", "mid", 2, running, 9), j("b1", "bob", "high", 2, pending, 8)},
			map[string]string{"p2": "running", "b1": "pending"},
			map[string]string{"p2": "evicted", "b1": "placed"},
		},
		{
			"promoted gives way to its owner's P1", "2", map[string]int{"n0": 2},
			[]testJob{j("p2", "alice", "mid", 2, running, 9), j("a1", "alice", "high", 2, pending, 8)},
			map[string]string{"p2": "evicted", "a1": "placed"}, nil,
		},
		{
			"a P1 that does not fit leaves promotion alone", "4", map[string]int{"n0": 4},
			[]testJob{j("a1", "alice", "high", 2, running, 9), j("p2", "alice", "mid", 2, running, 9), j("big", "alice", "high", 4, pending, 8), j("b1", "bob", "high", 2, pending, 7)},
			map[string]string{"a1": "running", "p2": "running", "big": "pending", "b1": "pending"},
			map[string]string{"p2": "evicted", "b1": "placed"},
		},
		{
			"excess is evicted for a normal job, newest submission first", "0", map[string]int{"n0": 6},
			[]testJob{j("n1", "alice", "mid", 2, running, 9), j("n2", "alice", "mid", 2, running, 8), j("x", "alice", "mid", 2, running, 7), j("b", "bob", "mid", 2, pending, 6)},
			map[string]string{"n1": "running", "n2": "running", "x": "evicted", "b": "placed"},
			map[string]string{"x": "running", "b": "pending"},
		},
		{
			"excess does not evict excess", "0", map[string]int{"n0": 8},
			[]testJob{j("x", "alice", "mid", 8, running, 9), j("y", "bob", "mid", 8, pending, 8)},
			map[string]string{"x": "running", "y": "pending"}, nil,
		},
		{
			"excess still evicts lower classes", "0", map[string]int{"n0": 8},
			[]testJob{j("low", "carol", "low", 8, running, 9), j("y", "bob", "mid", 8, pending, 8)},
			map[string]string{"low": "evicted", "y": "placed"}, nil,
		},
		{
			"the exempt class evicts P1 and ignores budgets", "2", map[string]int{"n0": 2},
			[]testJob{j("a1", "alice", "high", 2, running, 9), j("p0", "alice", "top", 2, pending, 8)},
			map[string]string{"a1": "evicted", "p0": "placed"}, nil,
		},
		{
			"a placement that is rolled back uses no budget", "2", map[string]int{"n0": 1},
			[]testJob{{name: "gang", owner: "alice", class: "high", gpus: 1, pods: []pod_status.PodStatus{pending, pending}, age: 9}, j("a1", "alice", "high", 1, pending, 8)},
			map[string]string{"gang": "pending", "a1": "placed"}, nil,
		},
		{
			"fractions", "1", map[string]int{"n0": 4},
			[]testJob{j("a1", "alice", "high", 0.5, pending, 9), j("a2", "alice", "high", 0.5, pending, 8), j("a3", "alice", "high", 0.5, pending, 7)},
			map[string]string{"a1": "placed", "a2": "placed", "a3": "pending"},
			map[string]string{"a3": "placed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(ssn *framework.Session, want map[string]string) {
				t.Helper()
				for name, w := range want {
					if got := state(find(t, ssn, name)); got != w {
						t.Errorf("%s: got %s, want %s", name, got, w)
					}
				}
			}
			ssn := session(t, tc.nodes, tc.jobs)
			schedule(ssn, testArgs(tc.budget))
			check(ssn, tc.want)

			// Audit mode and an invalid configuration must both be stock KAI.
			audit := testArgs(tc.budget)
			audit["mode"] = "audit"
			for name, args := range map[string]framework.PluginArguments{"audit": audit, "invalid": {"mode": "x"}} {
				t.Run(name, func(t *testing.T) {
					stock := maps.Clone(tc.want)
					maps.Copy(stock, tc.stock)
					ssn := session(t, tc.nodes, tc.jobs)
					schedule(ssn, args)
					for job, w := range stock {
						if got := state(find(t, ssn, job)); got != w {
							t.Errorf("%s: got %s, want %s", job, got, w)
						}
					}
					if len(ssn.VictimInvariantPrePredicateFns) != 1 {
						t.Error("hooks were added")
					}
				})
			}
		})
	}
}

func TestHoldReasonReachesUser(t *testing.T) {
	ssn := session(t, map[string]int{"n0": 8}, []testJob{j("a1", "alice", "high", 2, running, 9), j("a2", "alice", "high", 1, pending, 5)})
	schedule(ssn, testArgs("2"))
	a2 := find(t, ssn, "a2")
	var text string
	for _, e := range a2.TasksFitErrors {
		text += e.Error()
	}
	if want := "user budget: needs 1 units, using 2 of 2"; !strings.Contains(text, want) {
		t.Errorf("%q lacks %q", text, want)
	}
}

func holdOf(p *plugin, info *podgroup_info.PodGroupInfo, status pod_status.PodStatus) string {
	for _, task := range info.GetAllPodsMap() {
		if task.Status == status {
			return p.hold(task)
		}
	}
	return "no such pod"
}

func TestReplacementsAndGrowth(t *testing.T) {
	for _, tc := range []struct {
		name string
		pods []pod_status.PodStatus
		held bool
	}{
		{"replaces a terminating pod", []pod_status.PodStatus{running, releasing, pending}, false},
		{"replaces all pods", []pod_status.PodStatus{releasing, releasing, pending, pending}, false},
		{"replaces a pod that is gone", []pod_status.PodStatus{running, pending}, false},
		{"grows as far as fits", []pod_status.PodStatus{running, pending, pending}, false},
		{"grows", []pod_status.PodStatus{running, running, pending}, true},
		{"starts too big", []pod_status.PodStatus{pending, pending, pending}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ssn := session(t, map[string]int{"n0": 8}, []testJob{{name: "a", owner: "alice", class: "high", gpus: 1, pods: tc.pods, age: 9}})
			p := open(ssn, testArgs("2"))
			if reason := holdOf(p, find(t, ssn, "a"), pending); (reason != "") != tc.held {
				t.Errorf("hold %q, want held=%v", reason, tc.held)
			}
		})
	}
}

func TestReleasingJobStillCounts(t *testing.T) {
	ssn := session(t, map[string]int{"n0": 8}, []testJob{j("a1", "alice", "high", 2, releasing, 9), j("a2", "alice", "high", 2, pending, 8)})
	if reason := holdOf(open(ssn, testArgs("2")), find(t, ssn, "a2"), pending); !strings.Contains(reason, "using 2 of 2") {
		t.Errorf("hold %q", reason)
	}
}

func TestWeightsByNodeSelector(t *testing.T) {
	args := testArgs("4")
	args["gpuTypeLabel"], args["gpuWeights"], args["defaultGPUWeight"] = "gpu-type", "small=1,big=4", "4"
	for selector, held := range map[string]bool{"small": false, "big": true, "unknown": true, "": true} {
		ssn := session(t, map[string]int{"n0": 8}, []testJob{j("a", "alice", "high", 2, pending, 9)})
		for _, task := range find(t, ssn, "a").GetAllPodsMap() {
			task.Pod.Spec.NodeSelector = map[string]string{"gpu-type": selector}
		}
		if reason := holdOf(open(ssn, args), find(t, ssn, "a"), pending); (reason != "") != held {
			t.Errorf("%q: hold %q, want held=%v", selector, reason, held)
		}
	}
}

func TestPriorities(t *testing.T) {
	jobs := []testJob{
		j("p1", "alice", "mid", 2, running, 9), j("p2", "alice", "mid", 2, running, 8),
		j("n1", "alice", "mid", 2, running, 7), j("n2", "alice", "mid", 2, running, 6),
		j("x", "alice", "mid", 2, running, 5), j("y", "alice", "mid", 2, pending, 4),
		j("b", "bob", "mid", 2, pending, 4), j("top", "bob", "top", 2, running, 4), j("low", "bob", "low", 2, running, 4),
	}
	want := map[string]int32{"p1": 75, "p2": 75, "n1": 50, "n2": 50, "x": 45, "y": 45, "b": 50, "top": 90, "low": 25}
	// Nothing carries over, so a restarted scheduler decides the same.
	for range 2 {
		tierLog, errorLog = &changeLog{}, &changeLog{}
		ssn := session(t, map[string]int{"n0": 16}, jobs)
		before := map[string]any{}
		for name := range want {
			before[name] = find(t, ssn, name).Preemptibility
		}
		open(ssn, testArgs("4"))
		for name, w := range want {
			if got := find(t, ssn, name); got.Priority != w || got.Preemptibility != before[name] {
				t.Errorf("%s: priority %d, want %d; preemptibility %v", name, got.Priority, w, got.Preemptibility)
			}
		}
	}
}

// Another session with the same jobs after P1 starts demotes them.
func TestDemotionWhenP1Starts(t *testing.T) {
	jobs := []testJob{j("a", "alice", "mid", 2, running, 9), j("b", "alice", "mid", 2, running, 8), j("c", "alice", "mid", 2, running, 7), j("d", "alice", "mid", 2, running, 6)}
	for _, tc := range []struct {
		extra []testJob
		want  []int32
	}{
		{nil, []int32{75, 75, 50, 50}},
		{[]testJob{j("p", "alice", "high", 4, running, 1)}, []int32{50, 50, 45, 45}},
	} {
		ssn := session(t, map[string]int{"n0": 16}, append(jobs[:4:4], tc.extra...))
		open(ssn, testArgs("4"))
		for i, name := range []string{"a", "b", "c", "d"} {
			if got := find(t, ssn, name).Priority; got != tc.want[i] {
				t.Errorf("%s: %d, want %d", name, got, tc.want[i])
			}
		}
	}
}

func TestPromotesLongestRunning(t *testing.T) {
	requeued := j("old", "alice", "mid", 2, running, 9)
	requeued.started = 1
	ssn := session(t, map[string]int{"n0": 8}, []testJob{requeued, j("new", "alice", "mid", 2, running, 5)})
	open(ssn, testArgs("2"))
	if o, n := find(t, ssn, "old").Priority, find(t, ssn, "new").Priority; o != 50 || n != 75 {
		t.Errorf("old %d, new %d", o, n)
	}
}

func TestGPUMemoryCountsWholeGPUs(t *testing.T) {
	ssn := session(t, map[string]int{"n0": 8}, []testJob{j("a", "alice", "high", 1, pending, 9)})
	info := find(t, ssn, "a")
	for _, task := range info.GetAllPodsMap() {
		task.GpuRequirement = *resource_info.NewGpuResourceRequirementWithMultiFraction(3, 0, 1024)
	}
	if reason := holdOf(open(ssn, testArgs("2")), info, pending); !strings.Contains(reason, "needs 3 units") {
		t.Errorf("hold %q", reason)
	}
}

func TestPanicLeavesStockBehaviour(t *testing.T) {
	t.Run("opening", func(t *testing.T) {
		ssn := session(t, map[string]int{"n0": 4}, []testJob{j("p2", "alice", "mid", 1, running, 9), j("a1", "alice", "high", 1, pending, 9)})
		bad := podgroup_info.NewPodGroupInfo("ns/bad")
		bad.PodGroup = find(t, ssn, "p2").PodGroup.DeepCopy()
		bad.GetAllPodsMap()["nil"] = nil
		ssn.ClusterInfo.PodGroupInfos["ns/bad"] = bad
		open(ssn, testArgs("0"))
		delete(ssn.ClusterInfo.PodGroupInfos, "ns/bad")
		if got := find(t, ssn, "p2").Priority; got != 50 {
			t.Errorf("priority %d", got)
		}
		allocate.New().Execute(ssn)
		if got := state(find(t, ssn, "a1")); got != "placed" {
			t.Errorf("a1 is %s", got)
		}
	})
	t.Run("checking", func(t *testing.T) {
		ssn := session(t, map[string]int{"n0": 4}, []testJob{j("a1", "alice", "high", 1, pending, 9)})
		p := open(ssn, testArgs("0"))
		p.jobs[find(t, ssn, "a1").UID].info = nil
		for _, task := range find(t, ssn, "a1").GetAllPodsMap() {
			if failure := p.victimInvariantPrePredicate(task); failure != nil {
				t.Error(failure.Err)
			}
		}
		allocate.New().Execute(ssn)
		if got := state(find(t, ssn, "a1")); got != "placed" {
			t.Errorf("a1 is %s", got)
		}
	})
}

func BenchmarkSession(b *testing.B) {
	infos := map[common_info.PodGroupID]*podgroup_info.PodGroupInfo{}
	t := &testing.T{}
	template := session(t, map[string]int{"n0": 8}, []testJob{j("a", "alice", "mid", 1, running, 9), j("b", "alice", "high", 1, pending, 9)})
	for i := range 1300 {
		src := find(t, template, []string{"a", "b"}[i%2])
		c := *src
		c.PodGroup = src.PodGroup.DeepCopy()
		c.PodGroup.Labels[ownerLabel] = fmt.Sprint("o", i%50)
		c.UID = common_info.PodGroupID(fmt.Sprint(i))
		infos[c.UID] = &c
	}
	var task *pod_info.PodInfo
	for _, x := range infos["1"].GetAllPodsMap() {
		clone := *x
		clone.Job = "1"
		clone.Pod = &v1.Pod{}
		task = &clone
	}
	ssn := &framework.Session{ClusterInfo: template.ClusterInfo}
	ssn.ClusterInfo.PodGroupInfos = infos
	p := open(ssn, testArgs("4"))
	b.Run("open", func(b *testing.B) {
		for b.Loop() {
			open(ssn, testArgs("4"))
		}
	})
	b.Run("check", func(b *testing.B) {
		for b.Loop() {
			p.hold(task)
		}
	})
}

package replay

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	enginev2 "github.com/kai-scheduler/KAI-scheduler/pkg/apis/scheduling/v2"
	enginev2alpha2 "github.com/kai-scheduler/KAI-scheduler/pkg/apis/scheduling/v2alpha2"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/conf"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/conf_util"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/plugins/snapshot"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

type job struct {
	name, user, class string
	gpus              int64
	node              string // if running
}

var args = framework.PluginArguments{
	"userLabel": "example.com/user", "budgetedClasses": "high", "promotableClasses": "mid",
	"budget": "2", "allowance": "2", "promotedPriority": "75", "excessPriority": "45",
}

func build(t *testing.T, nodes map[string]int64, jobs []job) *snapshot.Snapshot {
	t.Helper()
	initialize()
	c, err := conf_util.GetDefaultSchedulerConf()
	if err != nil {
		t.Fatal(err)
	}
	raw := &snapshot.RawKubernetesObjects{}
	for name, gpus := range nodes {
		capacity := v1.ResourceList{
			v1.ResourceCPU: resource.MustParse("64"), v1.ResourceMemory: resource.MustParse("64Gi"),
			v1.ResourcePods: resource.MustParse("100"), "nvidia.com/gpu": *resource.NewQuantity(gpus, resource.DecimalSI),
		}
		raw.Nodes = append(raw.Nodes, &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name)},
			Status: v1.NodeStatus{
				Capacity: capacity, Allocatable: capacity,
				Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
			},
		})
	}
	for name, value := range map[string]int32{"top": 90, "high": 75, "mid": 50, "low": 25} {
		raw.PriorityClasses = append(raw.PriorityClasses, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Value: value})
	}
	all := enginev2.QueueResource{Quota: -1, Limit: -1, OverQuotaWeight: 1}
	resources := &enginev2.QueueResources{GPU: all, CPU: all, Memory: all}
	raw.Queues = []*enginev2.Queue{
		{ObjectMeta: metav1.ObjectMeta{Name: "parent"}, Spec: enginev2.QueueSpec{Resources: resources}},
		{ObjectMeta: metav1.ObjectMeta{Name: "q"}, Spec: enginev2.QueueSpec{ParentQueue: "parent", Resources: resources}},
	}
	start := time.Now().Add(-time.Hour)
	for i, j := range jobs {
		created := metav1.NewTime(start.Add(time.Duration(i) * time.Minute))
		pg := &enginev2alpha2.PodGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name: j.name, Namespace: "ns", UID: types.UID("pg-" + j.name), CreationTimestamp: created,
				Labels: map[string]string{"example.com/user": j.user}, Annotations: map[string]string{},
			},
			Spec: enginev2alpha2.PodGroupSpec{MinMember: ptr.To[int32](1), Queue: "q", PriorityClassName: j.class},
		}
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: j.name, Namespace: "ns", UID: types.UID("pod-" + j.name), CreationTimestamp: created,
				Annotations: map[string]string{"pod-group-name": j.name},
			},
			Spec: v1.PodSpec{
				SchedulerName: "kai-scheduler", PriorityClassName: j.class, NodeName: j.node,
				Containers: []v1.Container{{Name: "main", Image: "x", Resources: v1.ResourceRequirements{
					Limits:   v1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(j.gpus, resource.DecimalSI)},
					Requests: v1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(j.gpus, resource.DecimalSI)},
				}}},
			},
			Status: v1.PodStatus{Phase: v1.PodPending},
		}
		if j.node != "" {
			pod.Status.Phase = v1.PodRunning
			pg.Annotations["kai.scheduler/last-start-timestamp"] = created.UTC().Format(time.RFC3339)
		}
		raw.PodGroups = append(raw.PodGroups, pg)
		raw.Pods = append(raw.Pods, pod)
	}
	return &snapshot.Snapshot{
		Config: c,
		SchedulerParams: &conf.SchedulerParams{
			SchedulerName: "kai-scheduler", PartitionParams: &conf.SchedulingNodePoolParams{},
			UseSchedulingSignatures: true, MaxNumberConsolidationPreemptees: 16, NumOfStatusRecordingWorkers: 1,
		},
		RawObjects: raw,
	}
}

func TestReplay(t *testing.T) {
	for _, tc := range []struct {
		name        string
		nodes       map[string]int64
		jobs        []job
		stock, with map[string]Action
		tiers       map[string]tiers.Tier
	}{
		{
			"held", map[string]int64{"n0": 8},
			[]job{{"a1", "alice", "high", 2, "n0"}, {"a2", "alice", "high", 1, ""}, {"b", "bob", "high", 1, ""}},
			map[string]Action{"ns/a2": Placed, "ns/b": Placed}, map[string]Action{"ns/b": Placed},
			map[string]tiers.Tier{"a1": tiers.Budget, "a2": tiers.Held, "b": tiers.Budget},
		},
		{
			"promoted is safe", map[string]int64{"n0": 2},
			[]job{{"p", "alice", "mid", 2, "n0"}, {"b", "bob", "high", 2, ""}},
			map[string]Action{"ns/p": Evicted, "ns/b": Placed}, map[string]Action{},
			map[string]tiers.Tier{"p": tiers.Promoted, "b": tiers.Budget},
		},
		{
			"excess is evicted", map[string]int64{"n0": 8},
			[]job{{"x", "alice", "mid", 8, "n0"}, {"b", "bob", "mid", 2, ""}},
			map[string]Action{}, map[string]Action{"ns/x": Evicted, "ns/b": Placed},
			map[string]tiers.Tier{"x": tiers.Excess, "b": tiers.Normal},
		},
		{
			"stock consolidation moves a P1, ours does not", map[string]int64{"n0": 2, "n1": 2},
			[]job{{"a", "alice", "high", 1, "n0"}, {"b", "bob", "high", 1, "n1"}, {"w", "carol", "low", 2, ""}},
			map[string]Action{"ns/w": Placed}, map[string]Action{}, nil, // and one is moved, see below
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := build(t, tc.nodes, tc.jobs)
			// Twice, as a run must not change the snapshot.
			for range 2 {
				stock, err := Stock(s)
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(tc.name, "stock consolidation") {
					moved := 0
					for name, a := range stock.Actions {
						if a == Moved {
							moved++
							delete(stock.Actions, name)
						}
					}
					if moved != 1 {
						t.Errorf("%d moved", moved)
					}
				}
				if !maps.Equal(stock.Actions, tc.stock) || stock.Status != nil {
					t.Errorf("stock: %v, want %v", stock.Actions, tc.stock)
				}
				with, err := With(s, args)
				if err != nil {
					t.Fatal(err)
				}
				if !maps.Equal(with.Actions, tc.with) {
					t.Errorf("with: %v, want %v", with.Actions, tc.with)
				}
				got := map[string]tiers.Tier{}
				for _, j := range with.Status.Jobs {
					got[j.Name] = j.Tier
				}
				if tc.tiers != nil && !maps.Equal(got, tc.tiers) {
					t.Errorf("tiers %v, want %v", got, tc.tiers)
				}
			}
		})
	}
}

// A snapshot of a cluster that already runs the plugin gives the same results.
func TestReplayOfOurOwn(t *testing.T) {
	s := build(t, map[string]int64{"n0": 8}, []job{{"a1", "alice", "high", 2, "n0"}, {"a2", "alice", "high", 1, ""}})
	other := maps.Clone(args)
	other["budget"], other["mode"] = "9", "enforce"
	s.Config = config(s.Config, other)
	stock, err := Stock(s)
	if err != nil || stock.Actions["ns/a2"] != Placed {
		t.Error(stock, err)
	}
	with, err := With(s, args)
	if err != nil || len(with.Actions) != 0 || with.Status.Budget != 2 {
		t.Error(with, err)
	}
	if strings.Count(fmt.Sprint(config(s.Config, args).Tiers), "tierbudget") != 1 {
		t.Error("configured twice")
	}
}

func TestErrors(t *testing.T) {
	s := build(t, map[string]int64{"n0": 8}, nil)
	if _, err := With(s, framework.PluginArguments{"budget": "x"}); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Error(err)
	}
	dir := t.TempDir()
	write := func(name, file, text string) string {
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		z := zip.NewWriter(f)
		w, err := z.Create(file)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(w, text)
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, path := range []string{
		filepath.Join(dir, "missing"), write("a", "other.json", "{}"),
		write("b", snapshot.SnapshotFileName, "{"), write("c", snapshot.SnapshotFileName, "{}"),
	} {
		if _, err := Load(path); err == nil {
			t.Errorf("%s: no error", path)
		}
	}
	text, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(write("d", snapshot.SnapshotFileName, string(text)))
	if err != nil || len(loaded.RawObjects.Nodes) != 1 {
		t.Error(err)
	}
}

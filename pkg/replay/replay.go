// Package replay runs one scheduling cycle on a KAI snapshot, offline: once as stock
// KAI and once with the plugin.
package replay

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	nrtfake "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned/fake"
	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clientfeatures "k8s.io/client-go/features"
	"k8s.io/client-go/kubernetes/fake"

	kaifake "github.com/kai-scheduler/KAI-scheduler/pkg/apis/client/clientset/versioned/fake"
	dra "github.com/kai-scheduler/KAI-scheduler/pkg/common/resources/dra_version_aware_client"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_status"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/cache"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/conf"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/conf_util"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/log"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/plugins"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/plugins/snapshot"

	"github.com/takayuki112/ukai-scheduler/pkg/tierbudget"
)

const stockConsolidation = "consolidation"

// Action is what happened to a job in the cycle.
type Action string

const (
	Placed  Action = "placed"
	Evicted Action = "evicted"
	Moved   Action = "moved" // evicted to be placed again at once
)

// Result is the outcome of one cycle.
type Result struct {
	// Actions is by "namespace/podgroup". Jobs to which nothing happened are not in it.
	Actions map[string]Action
	// Status is nil for stock KAI.
	Status *tierbudget.Status
}

var initialize = sync.OnceFunc(func() {
	// The node topology client's informer hangs on fakes with this on.
	if gates, ok := clientfeatures.FeatureGates().(interface {
		Set(clientfeatures.Feature, bool) error
	}); ok {
		_ = gates.Set(clientfeatures.WatchListClient, false)
	}
	// KAI's log has no off switch. Errors that matter are returned.
	stderr := os.Stderr
	if null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stderr = null
	}
	_ = log.InitLoggers(0, false)
	os.Stderr = stderr
	actions.InitDefaultActions()
	plugins.InitDefaultPlugins()
	framework.RegisterPluginBuilder(tierbudget.Name, tierbudget.New)
	framework.RegisterAction(tierbudget.NewConsolidation())
})

// Load reads a snapshot as KAI's snapshot plugin writes it.
func Load(path string) (*snapshot.Snapshot, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	f, err := z.Open(snapshot.SnapshotFileName)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := &snapshot.Snapshot{}
	if err := json.NewDecoder(f).Decode(s); err != nil {
		return nil, fmt.Errorf("%s: %w", snapshot.SnapshotFileName, err)
	}
	if s.Config == nil || s.SchedulerParams == nil || s.RawObjects == nil {
		return nil, fmt.Errorf("%s is incomplete", snapshot.SnapshotFileName)
	}
	return s, nil
}

// Stock runs the cycle without the plugin.
func Stock(s *snapshot.Snapshot) (*Result, error) { return run(s, nil) }

// With runs the cycle with the plugin, in enforce mode whatever the arguments say.
func With(s *snapshot.Snapshot, args framework.PluginArguments) (*Result, error) {
	enforce := framework.PluginArguments{}
	for k, v := range args {
		enforce[k] = v
	}
	enforce["mode"] = string(tierbudget.Enforce)
	if _, err := tierbudget.ParseConfig(enforce); err != nil {
		return nil, err
	}
	return run(s, enforce)
}

// config adapts the snapshot's configuration, whichever scheduler it is from.
func config(old *conf.SchedulerConfiguration, args framework.PluginArguments) *conf.SchedulerConfiguration {
	c := *old
	c.Tiers = nil
	for _, tier := range old.Tiers {
		var t conf.Tier
		for _, p := range tier.Plugins {
			if p.Name != tierbudget.Name {
				t.Plugins = append(t.Plugins, p)
			}
		}
		c.Tiers = append(c.Tiers, t)
	}
	from, to := tierbudget.ConsolidationName, stockConsolidation
	if args != nil {
		from, to = to, from
		c.Tiers = append(c.Tiers, conf.Tier{Plugins: []conf.PluginOption{{Name: tierbudget.Name, Arguments: args}}})
	}
	names := strings.Split(old.Actions, ",")
	for i := range names {
		if names[i] = strings.TrimSpace(names[i]); names[i] == from {
			names[i] = to
		}
	}
	c.Actions = strings.Join(names, ", ")
	return &c
}

func run(s *snapshot.Snapshot, args framework.PluginArguments) (*Result, error) {
	initialize()
	c := config(s.Config, args)
	todo, err := conf_util.GetActionsFromConfig(c)
	if err != nil {
		return nil, err
	}
	sc, stop, err := newCache(s)
	if err != nil {
		return nil, err
	}
	defer stop()
	params := *s.SchedulerParams
	ssn, err := framework.OpenSession(sc, c, &params, "replay", http.NewServeMux())
	if err != nil {
		return nil, err
	}
	defer framework.CloseSession(ssn)

	type taskKey struct{ job, task string }
	before := map[taskKey]pod_status.PodStatus{}
	for id, job := range ssn.ClusterInfo.PodGroupInfos {
		for taskID, task := range job.GetAllPodsMap() {
			before[taskKey{string(id), string(taskID)}] = task.Status
		}
	}
	for _, a := range todo {
		a.Execute(ssn)
	}

	result := &Result{Actions: map[string]Action{}}
	for id, job := range ssn.ClusterInfo.PodGroupInfos {
		var evicted, placed, held bool
		for taskID, task := range job.GetAllPodsMap() {
			old := before[taskKey{string(id), string(taskID)}]
			if task.Status == old {
				continue
			}
			held = held || pod_status.IsActiveAllocatedStatus(old)
			evicted = evicted || task.Status == pod_status.Releasing
			placed = placed || pod_status.IsActiveAllocatedStatus(task.Status)
		}
		name := job.Namespace + "/" + job.Name
		switch {
		case evicted:
			result.Actions[name] = Evicted
		case placed && held:
			result.Actions[name] = Moved
		case placed:
			result.Actions[name] = Placed
		}
	}
	if args != nil {
		if result.Status = tierbudget.LastStatus(); result.Status.Error != "" {
			return nil, fmt.Errorf("the plugin failed: %s", result.Status.Error)
		}
	}
	return result, nil
}

// newCache makes a scheduler cache of the snapshot's objects. Nothing it writes leaves
// the process.
func newCache(s *snapshot.Snapshot) (cache.Cache, func(), error) {
	raw := s.RawObjects
	core := concat(objects(raw.Pods), objects(raw.Nodes), objects(raw.PriorityClasses), objects(raw.ConfigMaps),
		objects(raw.PersistentVolumes), objects(raw.PersistentVolumeClaims), objects(raw.CSIStorageCapacities),
		objects(raw.StorageClasses), objects(raw.CSIDrivers), objects(raw.ResourceClaims),
		objects(raw.ResourceSlices), objects(raw.DeviceClasses))
	kai := concat(objects(raw.Queues), objects(raw.PodGroups), objects(raw.BindRequests), objects(raw.Topologies))

	kube := fake.NewClientset()
	for _, o := range core {
		if err := kube.Tracker().Add(o); err != nil {
			return nil, nil, err
		}
	}
	if d, ok := kube.Discovery().(*fakediscovery.FakeDiscovery); ok && s.Discovery != nil {
		d.FakedServerVersion = s.Discovery.ServerVersion
		kube.Resources = s.Discovery.Resources
	}
	client := dra.NewDRAAwareClient(kube)
	p := s.SchedulerParams
	sc, err := cache.New(&cache.SchedulerCacheParams{
		KubeClient:                  client,
		KAISchedulerClient:          kaifake.NewSimpleClientset(kai...),
		NRTClient:                   nrtfake.NewSimpleClientset(objects(raw.NodeResourceTopologies)...),
		SchedulerName:               p.SchedulerName,
		NodePoolParams:              p.PartitionParams,
		RestrictNodeScheduling:      p.RestrictSchedulingNodes,
		DetailedFitErrors:           p.DetailedFitErrors,
		ScheduleCSIStorage:          p.ScheduleCSIStorage,
		FullHierarchyFairness:       p.FullHierarchyFairness,
		AllowConsolidatingReclaim:   p.AllowConsolidatingReclaim,
		NumOfStatusRecordingWorkers: p.NumOfStatusRecordingWorkers,
		StuckInReleasingThreshold:   p.StuckInReleasingThreshold,
		DiscoveryClient:             client.Discovery(),
	})
	if err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	sc.Run(done)
	sc.WaitForCacheSync(done)
	return sc, func() { close(done) }, nil
}

// objects copies, so that one run does not affect the next.
func objects[T runtime.Object](list []T) []runtime.Object {
	out := make([]runtime.Object, len(list))
	for i, o := range list {
		out[i] = o.DeepCopyObject()
	}
	return out
}

func concat(lists ...[]runtime.Object) []runtime.Object {
	var out []runtime.Object
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

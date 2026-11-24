// Package tierbudget is a KAI scheduler plugin that limits how much each owner can
// run at a budgeted priority, and moves other running jobs up or down depending on
// how much of that budget is free.
//
// It changes only KAI's in-memory copy of a job's priority, which KAI rebuilds each
// session, and writes nothing to the API.
package tierbudget

import (
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/common_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_status"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/podgroup_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/log"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// Name is the plugin's name in the scheduler configuration.
const Name = "tierbudget"

// KAI builds the plugin anew each session. These only keep logs short.
var (
	logLoaded sync.Once
	tierLog   = &changeLog{}
	errorLog  = &changeLog{}
)

// lastGroups remembers PodGroups for their orphans. After a restart, the pods' own label
// and class have to do.
var lastGroups atomic.Pointer[map[common_info.PodGroupID]group]

type group struct{ owner, className string }

type plugin struct {
	config *Config // nil if invalid
	err    error   // why
	ssn    *framework.Session
	jobs   map[common_info.PodGroupID]*jobState
	// results is set once they apply to the session.
	results map[string]tiers.Result
	// charged lists each owner's jobs that count against the budget.
	charged map[string][]*jobState
	// orphans are pods without a PodGroup, by its former ID.
	orphans map[common_info.PodGroupID]*orphan
}

type jobState struct {
	info  *podgroup_info.PodGroupInfo
	class tiers.Class
	owner string
	// units is the charge for the whole job when the session opened.
	units tiers.Units
	// running is whether it held resources when the session opened.
	running bool
}

// orphan is what is left of a job whose PodGroup is gone. Deleting a Job deletes its
// PodGroup at once, but the pods keep their GPUs for as long as they terminate. KAI
// still has them on their nodes, where nothing can change them.
type orphan struct {
	class     tiers.Class
	className string
	owner     string
	priority  int32
	units     tiers.Units
	created   time.Time
}

// New builds the plugin. It is a framework.PluginBuilder. With invalid arguments the
// plugin does nothing, which leaves stock KAI behaviour.
func New(args framework.PluginArguments) framework.Plugin {
	p := &plugin{}
	config, err := ParseConfig(args)
	if err != nil {
		if errorLog.changed("config", err.Error()) {
			log.InfraLogger.Errorf("Plugin %s is disabled, invalid configuration: %v", Name, err)
		}
		p.err = err
		return p
	}
	if errorLog.changed("config", "") {
		log.InfraLogger.Infof("Plugin %s configuration: %+v", Name, *config)
	}
	p.config = config
	return p
}

func (*plugin) Name() string { return Name }

func (p *plugin) OnSessionOpen(ssn *framework.Session) {
	logLoaded.Do(func() { log.InfraLogger.Infof("Plugin %s loaded", Name) })
	// Once, because KAI's server reads its handlers without a lock: adding one while
	// another request is served may crash the process.
	addEndpoint.Do(func() { ssn.AddHttpHandler(StatusPath, serveStatus) })
	if p.config == nil {
		p.publishError("invalid configuration: " + p.err.Error())
		return
	}
	defer recoverTo("opening the session", func() { p.publishError("panic while opening the session") })

	p.ssn = ssn
	p.collect()
	results := tiers.Assign(p.config.Policy, p.tierJobs())
	p.report(results)
	status := p.buildStatus(results)
	if p.config.Mode != Enforce {
		publish(status)
		return
	}
	warnStockConsolidation(ssn)
	// Nothing is applied until everything above has succeeded.
	for id, state := range p.jobs {
		state.info.Priority = results[string(id)].Priority
	}
	p.results = results
	openSession.Store(p)
	ssn.AddPrePredicateFn(p.prePredicate)
	ssn.AddVictimInvariantPrePredicateFn(p.victimInvariantPrePredicate)
	publish(status)
}

func (p *plugin) OnSessionClose(*framework.Session) {
	openSession.CompareAndSwap(p, nil)
	p.ssn, p.jobs, p.charged, p.orphans, p.results = nil, nil, nil, nil, nil
}

// collect reads the jobs that the configuration covers.
func (p *plugin) collect() {
	p.jobs = map[common_info.PodGroupID]*jobState{}
	p.charged = map[string][]*jobState{}
	for id, info := range p.ssn.ClusterInfo.PodGroupInfos {
		if info == nil || info.PodGroup == nil {
			continue
		}
		class := p.config.Classes[info.PodGroup.Spec.PriorityClassName]
		if class == tiers.Other {
			continue
		}
		u := p.usage(info)
		state := &jobState{
			info:    info,
			class:   class,
			owner:   info.PodGroup.Labels[p.config.OwnerLabel],
			units:   u.whole(),
			running: u.active,
		}
		p.jobs[id] = state
		if class == tiers.Budgeted || class == tiers.Charged {
			p.charged[state.owner] = append(p.charged[state.owner], state)
		}
	}
	p.collectOrphans()
}

func (p *plugin) collectOrphans() {
	known := map[common_info.PodGroupID]group{}
	for id, info := range p.ssn.ClusterInfo.PodGroupInfos {
		if info != nil && info.PodGroup != nil {
			known[id] = group{info.PodGroup.Labels[p.config.OwnerLabel], info.PodGroup.Spec.PriorityClassName}
		}
	}
	var before map[common_info.PodGroupID]group
	if last := lastGroups.Load(); last != nil {
		before = *last
	}
	p.orphans = map[common_info.PodGroupID]*orphan{}
	for _, node := range p.ssn.ClusterInfo.Nodes {
		for _, task := range node.PodInfos {
			if task.Job == "" || task.Pod == nil || !pod_status.IsActiveUsedStatus(task.Status) ||
				p.ssn.ClusterInfo.PodGroupInfos[task.Job] != nil {
				continue
			}
			g, ok := before[task.Job]
			if !ok {
				g = group{task.Pod.Labels[p.config.OwnerLabel], task.Pod.Spec.PriorityClassName}
			}
			known[task.Job] = g
			class := p.config.Classes[g.className]
			if class == tiers.Other {
				continue
			}
			o := p.orphans[task.Job]
			if o == nil {
				o = &orphan{class: class, className: g.className, owner: g.owner, created: task.Pod.CreationTimestamp.Time}
				if task.Pod.Spec.Priority != nil {
					o.priority = *task.Pod.Spec.Priority
				}
				p.orphans[task.Job] = o
			}
			o.units += p.taskUnits(task)
		}
	}
	lastGroups.Store(&known)
}

func (p *plugin) tierJobs() []tiers.Job {
	jobs := make([]tiers.Job, 0, len(p.jobs))
	for id, s := range p.jobs {
		j := tiers.Job{
			ID: string(id), Owner: s.owner, Class: s.class, Units: s.units,
			Priority: s.info.Priority, Running: s.running, Created: s.info.CreationTimestamp.Time,
		}
		// KAI records the start in whole seconds, and not for jobs it did not start.
		j.Started = j.Created
		if s.info.LastStartTimestamp != nil {
			j.Started = s.info.LastStartTimestamp.Truncate(time.Second)
		}
		jobs = append(jobs, j)
	}
	for id, o := range p.orphans {
		jobs = append(jobs, tiers.Job{
			ID: string(id), Owner: o.owner, Class: o.class, Units: o.units,
			Priority: o.priority, Running: true, Created: o.created, Started: o.created,
		})
	}
	return jobs
}

// usage is a job's units, by what its pods are doing.
type usage struct {
	alive     tiers.Units // pending or holding resources
	allocated tiers.Units // holding resources, and not releasing them
	held      tiers.Units // holding or releasing resources
	active    bool        // any pod holds or releases resources
}

// whole charges a job once, so a replacement does not count in addition to the
// terminating pod that it replaces.
func (u usage) whole() tiers.Units { return max(u.alive, u.held) }

// usage reads the current statuses, which KAI also updates for simulated placements
// and restores when it rolls them back.
func (p *plugin) usage(info *podgroup_info.PodGroupInfo) usage {
	var u usage
	for _, task := range info.GetAllPodsMap() {
		n := p.taskUnits(task)
		if pod_status.IsAliveStatus(task.Status) {
			u.alive += n
		}
		if pod_status.IsActiveAllocatedStatus(task.Status) {
			u.allocated += n
		}
		if pod_status.IsActiveUsedStatus(task.Status) {
			u.held += n
			u.active = true
		}
	}
	return u
}

func (p *plugin) taskUnits(task *pod_info.PodInfo) tiers.Units {
	gpus := task.GpuRequirement.GetGpusQuota()
	if task.GpuRequirement.GpuMemory() > 0 {
		// The share of a GPU depends on the node, so charge whole ones.
		gpus = float64(task.GpuRequirement.GetNumOfGpuDevices())
	}
	var selector map[string]string
	if task.Pod != nil {
		selector = task.Pod.Spec.NodeSelector
	}
	return p.config.Units(gpus, selector)
}

// hold says why a task may not be placed, or "" if it may.
func (p *plugin) hold(task *pod_info.PodInfo) string {
	job := p.jobs[task.Job]
	if job == nil || job.class != tiers.Budgeted {
		return ""
	}
	u := p.usage(job.info)
	need := u.whole()
	if job.running {
		// Pods that a running job adds must fit too, but a replacement takes over
		// the charge of the pod it replaces.
		need = max(u.allocated, u.held)
		if !pod_status.IsActiveAllocatedStatus(task.Status) {
			need = max(u.allocated+p.taskUnits(task), u.held)
		}
	}
	if need == 0 || p.taskUnits(task) == 0 {
		return ""
	}
	if job.owner == "" {
		if job.running {
			return ""
		}
		return fmt.Sprintf("no user: label %s is not set", p.config.OwnerLabel)
	}
	var used tiers.Units
	for _, other := range p.charged[job.owner] {
		if other != job {
			if o := p.usage(other.info); o.active {
				used += o.whole()
			}
		}
	}
	for _, o := range p.orphans {
		if o.owner == job.owner && o.class != tiers.Promotable {
			used += o.units
		}
	}
	return p.config.Policy.OverBudget(need, used)
}

func (p *plugin) prePredicate(task *pod_info.PodInfo, _ *podgroup_info.PodGroupInfo) (err error) {
	defer recoverTo("checking a task", func() { err = nil })
	if reason := p.hold(task); reason != "" {
		return holdError(reason)
	}
	return nil
}

// victimInvariantPrePredicate stops KAI from looking for victims for a held job.
// Budgeted jobs do not evict each other to get within budget, and an evicted one
// counts until it is gone, so no choice of victims would help. It also keeps KAI from
// concluding that similar jobs of other owners cannot be placed either.
func (p *plugin) victimInvariantPrePredicate(task *pod_info.PodInfo) (failure *api.VictimInvariantPrePredicateFailure) {
	defer recoverTo("checking a task", func() { failure = nil })
	if reason := p.hold(task); reason != "" {
		return &api.VictimInvariantPrePredicateFailure{Err: holdError(reason)}
	}
	return nil
}

func holdError(reason string) error { return fmt.Errorf("user budget: %s", reason) }

// report logs jobs whose tier changed.
func (p *plugin) report(results map[string]tiers.Result) {
	current := make(map[string]string, len(results))
	for id, r := range results {
		current[id] = fmt.Sprintf("%s (priority %d) %s", r.Tier, r.Priority, r.Reason)
	}
	for id, text := range tierLog.replace(current) {
		var owner string
		if job := p.jobs[common_info.PodGroupID(id)]; job != nil {
			owner = job.owner
		} else if o := p.orphans[common_info.PodGroupID(id)]; o != nil {
			owner, text = o.owner, text+" (its PodGroup is gone)"
		}
		log.InfraLogger.Infof("Plugin %s (%s): job %s of user %q is %s", Name, p.config.Mode, id, owner, text)
	}
}

// recoverTo stops a panic, so that a bug here leaves stock KAI behaviour instead of
// stopping the scheduler. Deferred functions run it, so recover works.
func recoverTo(doing string, then func()) {
	r := recover()
	if r == nil {
		return
	}
	panicCount.Inc()
	if errorLog.changed("panic", fmt.Sprint(doing, r)) {
		log.InfraLogger.Errorf("Plugin %s: panic while %s: %v\n%s", Name, doing, r, debug.Stack())
	}
	if then != nil {
		then()
	}
}

// changeLog remembers what was last logged.
type changeLog struct {
	mu   sync.Mutex
	last map[string]string
}

// changed records a value and says whether it differs from the last one.
func (c *changeLog) changed(key, value string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.last[key]
	if ok && old == value {
		return false
	}
	if c.last == nil {
		c.last = map[string]string{}
	}
	c.last[key] = value
	return true
}

// replace records all values, forgets the others, and returns those that differ.
func (c *changeLog) replace(values map[string]string) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	diff := map[string]string{}
	for k, v := range values {
		if old, ok := c.last[k]; !ok || old != v {
			diff[k] = v
		}
	}
	c.last = values
	return diff
}

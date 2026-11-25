// Package dashboard shows users their jobs, their budget and the queue of P1. It reads
// the API with the user's own rights, and computes tiers with the scheduler's rules.
package dashboard

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"

	enginev2alpha2 "github.com/kai-scheduler/KAI-scheduler/pkg/apis/scheduling/v2alpha2"
	"github.com/kai-scheduler/KAI-scheduler/pkg/common/constants"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_status"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/resource_info"

	"github.com/takayuki112/ukai-scheduler/pkg/tierbudget"
	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// Data is what the API says.
type Data struct {
	Groups []*enginev2alpha2.PodGroup
	Pods   []*v1.Pod
	// GPUMemory is that of each GPU of a node, in MiB.
	GPUMemory map[string]int64
	// SeesPods says whether a namespace's pods are in Pods.
	SeesPods func(namespace string) bool
	// Former is what is known of PodGroups that may be gone, by "namespace/name".
	Former map[string]Group
	// Notes say what is missing. Lost is set while the connection is.
	Notes []string
	Lost  string
}

// Group is what decides the tier of a PodGroup's remains.
type Group struct{ User, ClassName string }

// Types of GPU that are not names.
const (
	anyType     = "any type"
	unknownType = "type not visible"
)

// Job is one PodGroup.
type Job struct {
	ID, User, ClassName, Queue string
	Tier                       tiers.Tier
	Units                      tiers.Units
	Estimated                  bool // from the PodGroup alone
	Running                    bool
	Gone                       bool // its PodGroup is
	Types                      []string
	Created, Since             time.Time
	Reason                     string

	class tiers.Class
}

// Queue is the waiting P1 for a type of GPU, in order.
type Queue struct {
	Type string
	Jobs []*Job
}

// View is what is shown.
type View struct {
	User         string
	Audit        bool
	Used, Budget tiers.Units
	Jobs         []*Job // the user's, by tier
	Queues       []Queue
	Notes        []string
	Lost         string
}

var tierOrder = []tiers.Tier{tiers.Budget, tiers.Held, tiers.Promoted, tiers.Normal, tiers.Excess}

// heldReason is how the plugin says why it holds a job. Only this is taken from messages.
var heldReason = regexp.MustCompile(`user budget: (needs \d+(?:\.\d+)? units, (?:using \d+(?:\.\d+)? of|budget is) \d+(?:\.\d+)?|no user: label \S+ is not set)`)

// UserOf is the user that a PodGroup or pod belongs to.
func UserOf(config *tierbudget.Config, labels map[string]string) string {
	return labels[config.OwnerLabel]
}

// usage follows the plugin's.
type usage struct {
	alive, held tiers.Units
	active      bool
	types       []string
	created     time.Time
	started     time.Time
	reason      string
}

// Compute works out what to show a user. It does not modify the data.
func Compute(config *tierbudget.Config, user string, d Data) *View {
	v := &View{User: user, Audit: config.Mode != tierbudget.Enforce, Budget: config.Policy.Budget, Notes: slices.Clone(d.Notes), Lost: d.Lost}
	uses := map[string]*usage{}
	former := map[string]Group{}
	vectors := resource_info.NewResourceVectorMap()
	var claims, wholeShares bool
	for _, pod := range d.Pods {
		task := pod_info.NewTaskInfo(pod, vectors)
		if task.Job == "" {
			continue
		}
		id := string(task.Job)
		u := uses[id]
		if u == nil {
			u = &usage{created: pod.CreationTimestamp.Time}
			uses[id] = u
			former[id] = Group{UserOf(config, pod.Labels), pod.Spec.PriorityClassName}
		}
		memory := d.GPUMemory[task.NodeName]
		n := config.TaskUnits(task, memory)
		if pod_status.IsAliveStatus(task.Status) {
			u.alive += n
		}
		if pod_status.IsActiveUsedStatus(task.Status) {
			u.held += n
			u.active = true
			if task.GpuRequirement.GpuMemory() > 0 && memory == 0 && pod.Annotations[constants.GpuFraction] != "" {
				wholeShares = true
			}
		}
		if n > 0 && (pod_status.IsAliveStatus(task.Status) || pod_status.IsActiveUsedStatus(task.Status)) {
			if t := gpuType(config, pod.Spec.NodeSelector); !slices.Contains(u.types, t) {
				u.types = append(u.types, t)
			}
		}
		if len(pod.Spec.ResourceClaims) > 0 {
			claims = true
		}
		if pod.CreationTimestamp.Time.Before(u.created) {
			u.created = pod.CreationTimestamp.Time
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == v1.PodScheduled && c.Status == v1.ConditionFalse {
				u.reason = cmp.Or(u.reason, reasonIn(c.Message))
			}
		}
	}

	jobs := map[string]*Job{}
	var blind []string
	for _, g := range d.Groups {
		id := g.Namespace + "/" + g.Name
		j := &Job{
			ID: id, User: UserOf(config, g.Labels), ClassName: g.Spec.PriorityClassName, Queue: g.Spec.Queue,
			class: config.Classes[g.Spec.PriorityClassName], Created: g.CreationTimestamp.Time,
		}
		if j.class == tiers.Other {
			delete(uses, id) // it is no orphan
			continue
		}
		if d.SeesPods(g.Namespace) {
			if u := uses[id]; u != nil {
				j.Units, j.Running, j.Types, j.Reason = max(u.alive, u.held), u.active, u.types, u.reason
			}
		} else {
			estimate(config, g, j)
			if j.User == user && j.Units > 0 && !slices.Contains(blind, g.Namespace) {
				blind = append(blind, g.Namespace)
			}
		}
		delete(uses, id)
		for _, c := range g.Status.SchedulingConditions {
			if c.Status == v1.ConditionTrue {
				j.Reason = cmp.Or(reasonIn(c.Message), j.Reason)
			}
		}
		j.Since = j.Created
		if j.Running {
			j.Reason = ""
			if t, err := time.Parse(time.RFC3339, g.Annotations[constants.LastStartTimeStamp]); err == nil {
				j.Since = t
			}
		}
		jobs[id] = j
	}
	for id, u := range uses { // PodGroups that are gone
		g, ok := d.Former[id]
		if !ok {
			g = former[id]
		}
		if class := config.Classes[g.ClassName]; u.active && class != tiers.Other {
			jobs[id] = &Job{
				ID: id, User: g.User, ClassName: g.ClassName, class: class, Units: u.held, Running: true, Gone: true,
				Types: u.types, Created: u.created, Since: u.created,
			}
		}
	}

	input := make([]tiers.Job, 0, len(jobs))
	for id, j := range jobs {
		input = append(input, tiers.Job{ID: id, Owner: j.User, Class: j.class, Units: j.Units, Running: j.Running, Created: j.Created, Started: j.Since})
	}
	for id, r := range tiers.Assign(config.Policy, input) {
		j := jobs[id]
		j.Tier = r.Tier
		if j.class == tiers.Budgeted && !j.Running && j.Reason != "" {
			j.Tier = tiers.Held // the scheduler says so
		} else if r.Tier == tiers.Held {
			j.Reason = r.Reason
		} else {
			j.Reason = ""
		}
	}

	queues := map[string][]*Job{}
	for _, j := range jobs {
		if j.Units == 0 {
			continue
		}
		if j.User == user {
			v.Jobs = append(v.Jobs, j)
			if j.Running && j.class != tiers.Promotable {
				v.Used += j.Units
			}
		}
		if j.class == tiers.Budgeted && !j.Running {
			for _, t := range j.Types {
				queues[t] = append(queues[t], j)
			}
		}
	}
	order := func(a, b *Job) int { return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID)) }
	slices.SortFunc(v.Jobs, func(a, b *Job) int {
		return cmp.Or(cmp.Compare(slices.Index(tierOrder, a.Tier), slices.Index(tierOrder, b.Tier)), order(a, b))
	})
	for t, q := range queues {
		slices.SortFunc(q, order)
		v.Queues = append(v.Queues, Queue{t, q})
	}
	slices.SortFunc(v.Queues, func(a, b Queue) int { return cmp.Compare(a.Type, b.Type) })

	if len(blind) > 0 {
		slices.Sort(blind)
		v.Notes = append(v.Notes, "~ is an estimate. To see pods there, add -n "+strings.Join(blind, ","))
	}
	if claims {
		v.Notes = append(v.Notes, "GPUs requested by resource claims are not counted here, though the scheduler counts them.")
	}
	if wholeShares {
		v.Notes = append(v.Notes, "Running shares of a GPU count as whole ones here, as the GPU memory of nodes is not visible.")
	}
	return v
}

func gpuType(config *tierbudget.Config, selector map[string]string) string {
	if config.GPUTypeLabel == "" {
		return anyType
	}
	return cmp.Or(selector[config.GPUTypeLabel], anyType)
}

// estimate reads what KAI's pod-group controller adds up. The types are not in it.
func estimate(config *tierbudget.Config, g *enginev2alpha2.PodGroup, j *Job) {
	gpus := func(l v1.ResourceList) float64 {
		q := l[v1.ResourceName(constants.NvidiaGpuResource)]
		return q.AsApproximateFloat64()
	}
	s := g.Status.ResourcesStatus
	allocated := gpus(s.Allocated)
	j.Units = config.Units(max(allocated, gpus(s.Requested)), nil)
	j.Running, j.Estimated, j.Types = allocated > 0, true, []string{unknownType}
}

func reasonIn(message string) string {
	if m := heldReason.FindStringSubmatch(message); m != nil {
		return m[1]
	}
	return ""
}

// parseGPUMemory follows KAI in taking huge values for bytes.
func parseGPUMemory(label string) int64 {
	n, err := strconv.ParseInt(label, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	if n < 1<<20 {
		return n
	}
	return n >> 20
}

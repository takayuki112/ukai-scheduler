package tierbudget

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// StatusPath is where KAI's plugin server, on localhost, serves the status.
const StatusPath = "/tiers"

// Status is the outcome of the last session, when it opened. It is not modified once
// published.
type Status struct {
	Time time.Time `json:"time"`
	Mode Mode      `json:"mode,omitempty"`
	// Error says why stock behaviour applies. There are then no jobs.
	Error  string      `json:"error,omitempty"`
	Budget float64     `json:"budget"`
	Jobs   []JobStatus `json:"jobs"`

	jobMetrics bool
}

// JobStatus is a managed job. Jobs without GPUs are left out.
type JobStatus struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	Class     string `json:"class"`
	Running   bool   `json:"running"`
	// Orphan is set if pods are left but the PodGroup is gone.
	Orphan bool       `json:"orphan,omitempty"`
	Units  float64    `json:"units"`
	Tier   tiers.Tier `json:"tier"`
	// Priority is the effective one: in audit mode, what it would be.
	Priority      int32  `json:"priority"`
	ClassPriority int32  `json:"classPriority"`
	Reason        string `json:"reason,omitempty"`
}

var (
	lastStatus  atomic.Pointer[Status]
	addEndpoint sync.Once

	tierChanges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ukai_tier_changes_total",
		Help: "Changes of jobs' effective tier",
	}, []string{"from", "to"})
	panicCount = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ukai_panics_total",
		Help: "Panics after which stock behaviour applied",
	})
)

func init() { prometheus.MustRegister(tierChanges, panicCount, collector{}) }

// LastStatus returns the status of the last session, or nil before the first.
func LastStatus() *Status { return lastStatus.Load() }

// buildStatus must run before priorities are changed.
func (p *plugin) buildStatus(results map[string]tiers.Result) *Status {
	s := &Status{
		Time: time.Now(), Mode: p.config.Mode, Budget: p.config.Policy.Budget.Float(),
		Jobs: []JobStatus{}, jobMetrics: p.config.JobMetrics,
	}
	for id, job := range p.jobs {
		r := results[string(id)]
		if r.Tier == tiers.Unmanaged {
			continue
		}
		s.Jobs = append(s.Jobs, JobStatus{
			Namespace: job.info.Namespace, Name: job.info.Name, Owner: job.owner,
			Class: job.info.PodGroup.Spec.PriorityClassName, Running: job.running, Units: job.units.Float(),
			Tier: r.Tier, Priority: r.Priority, ClassPriority: job.info.Priority, Reason: r.Reason,
		})
	}
	for id, o := range p.orphans {
		r := results[string(id)]
		if r.Tier == tiers.Unmanaged {
			continue
		}
		namespace, name, _ := strings.Cut(string(id), "/")
		s.Jobs = append(s.Jobs, JobStatus{
			Namespace: namespace, Name: name, Owner: o.owner, Class: o.className, Running: true, Orphan: true,
			Units: o.units.Float(), Tier: r.Tier, Priority: r.Priority, ClassPriority: o.priority,
		})
	}
	slices.SortFunc(s.Jobs, func(a, b JobStatus) int {
		return cmp.Or(cmp.Compare(a.Owner, b.Owner), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return s
}

func publish(s *Status) {
	old := lastStatus.Swap(s)
	if old == nil {
		return
	}
	type key struct{ namespace, name string }
	before := make(map[key]tiers.Tier, len(old.Jobs))
	for _, j := range old.Jobs {
		before[key{j.Namespace, j.Name}] = j.Tier
	}
	for _, j := range s.Jobs {
		if from, ok := before[key{j.Namespace, j.Name}]; ok && from != j.Tier {
			tierChanges.WithLabelValues(string(from), string(j.Tier)).Inc()
		}
	}
}

// publishError reports that stock behaviour applies.
func (p *plugin) publishError(text string) {
	s := &Status{Time: time.Now(), Error: text, Jobs: []JobStatus{}}
	if p.config != nil {
		s.Mode = p.config.Mode
	}
	publish(s)
}

// serveStatus writes the status as JSON. The query may name an owner.
func serveStatus(w http.ResponseWriter, r *http.Request) {
	s := lastStatus.Load()
	if s == nil {
		http.Error(w, "no session yet", http.StatusServiceUnavailable)
		return
	}
	out := *s
	if q := r.URL.Query(); q.Has("owner") {
		out.Jobs = []JobStatus{}
		for _, j := range s.Jobs {
			if j.Owner == q.Get("owner") {
				out.Jobs = append(out.Jobs, j)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

var (
	activeDesc = prometheus.NewDesc("ukai_active",
		"1 if the plugin works, 0 if stock behaviour applies because of an error", []string{"mode"}, nil)
	sessionDesc = prometheus.NewDesc("ukai_last_session_timestamp_seconds",
		"When the last session opened", nil, nil)
	budgetDesc = prometheus.NewDesc("ukai_owner_budget_units", "Budget of each owner", nil, nil)
	jobsDesc   = prometheus.NewDesc("ukai_jobs", "Jobs by effective tier", []string{"owner", "tier", "state"}, nil)
	unitsDesc  = prometheus.NewDesc("ukai_units", "Units of jobs by effective tier", []string{"owner", "tier", "state"}, nil)
	jobDesc    = prometheus.NewDesc("ukai_job_priority", "Effective priority of each job",
		[]string{"namespace", "podgroup", "owner", "tier"}, nil)
)

// collector derives gauges from the last status, so that jobs that are gone leave
// nothing behind.
type collector struct{}

func (collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{activeDesc, sessionDesc, budgetDesc, jobsDesc, unitsDesc, jobDesc} {
		ch <- d
	}
}

func (collector) Collect(ch chan<- prometheus.Metric) {
	// Prometheus collects in a goroutine where a panic would end the process.
	defer recoverTo("collecting metrics", nil)
	s := lastStatus.Load()
	if s == nil {
		return
	}
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	gauge(sessionDesc, float64(s.Time.UnixMilli())/1000)
	if s.Error != "" {
		gauge(activeDesc, 0, string(s.Mode))
		return
	}
	gauge(activeDesc, 1, string(s.Mode))
	gauge(budgetDesc, s.Budget)

	type key struct{ owner, tier, state string }
	jobs, units := map[key]float64{}, map[key]float64{}
	for _, j := range s.Jobs {
		k := key{j.Owner, string(j.Tier), "pending"}
		if j.Running {
			k.state = "running"
		}
		jobs[k]++
		units[k] += j.Units
		if s.jobMetrics {
			gauge(jobDesc, float64(j.Priority), j.Namespace, j.Name, j.Owner, string(j.Tier))
		}
	}
	for k, n := range jobs {
		gauge(jobsDesc, n, k.owner, k.tier, k.state)
		gauge(unitsDesc, units[k], k.owner, k.tier, k.state)
	}
}

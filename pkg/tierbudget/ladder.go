package tierbudget

import (
	"strconv"
	"strings"
	"sync"
	"time"

	enginev2alpha2 "github.com/kai-scheduler/KAI-scheduler/pkg/apis/scheduling/v2alpha2"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/common_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/podgroup_info"

	"github.com/takayuki112/ukai-scheduler/pkg/ladder"
	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// activeUsers remembers when each user last had a laddered job, for the share. After a
// restart it starts empty, so the share is larger for up to the window.
var activeUsers = struct {
	sync.Mutex
	seen map[string]time.Time
}{seen: map[string]time.Time{}}

// ladderOutcome is what the ladder scheme adds to a session.
type ladderOutcome struct {
	results map[string]ladder.Result
	// share is the fair share in units, or 0 with widths in units.
	share tiers.Units
}

// assignLadder decides priorities by the ladder.
func (p *plugin) assignLadder(now time.Time) (map[string]tiers.Result, *ladderOutcome) {
	cfg := p.config.Ladder
	jobs := p.ladderJobs()
	cluster := p.clusterUnits()
	policy := ladder.Policy{
		Rungs: cfg.Rungs, ProtectCost: cfg.ProtectCost, ProtectLimit: cfg.ProtectLimit, ProtectFor: cfg.ProtectFor,
		ProtectCluster: tiers.UnitsOf(cfg.ProtectCluster * cluster.Float()),
	}
	out := &ladderOutcome{}
	if cfg.Share {
		out.share = shareOf(cluster, countActive(jobs, now, cfg.ShareWindow))
		policy.Rungs = ladder.Scale(cfg.Rungs, out.share)
		policy.ProtectLimit = tiers.UnitsOf(cfg.ProtectLimit.Float() * out.share.Float())
	}
	out.results = ladder.Assign(policy, jobs, now)
	results := make(map[string]tiers.Result, len(out.results))
	for id, r := range out.results {
		if r.Tier == tiers.Unmanaged {
			r.Priority = p.classPriority(id)
		}
		results[id] = r.Result
		out.results[id] = r
	}
	return results, out
}

func shareOf(cluster tiers.Units, users int) tiers.Units {
	return cluster / tiers.Units(max(users, 1))
}

// countActive records the users with jobs now and counts those seen within the window.
func countActive(jobs []ladder.Job, now time.Time, window time.Duration) int {
	activeUsers.Lock()
	defer activeUsers.Unlock()
	for _, j := range jobs {
		if j.User != "" && j.Units > 0 {
			activeUsers.seen[j.User] = now
		}
	}
	n := 0
	for user, at := range activeUsers.seen {
		if now.Sub(at) > window {
			delete(activeUsers.seen, user)
			continue
		}
		n++
	}
	return n
}

// clusterUnits is the weight of all GPUs on all nodes, by each node's GPU type.
func (p *plugin) clusterUnits() tiers.Units {
	var total tiers.Units
	for _, node := range p.ssn.ClusterInfo.Nodes {
		if node == nil || node.Node == nil {
			continue
		}
		total += p.config.Units(float64(node.GetNumberOfGPUsInNode()), node.Node.Labels)
	}
	return total
}

func (p *plugin) classPriority(id string) int32 {
	if s := p.jobs[common_info.PodGroupID(id)]; s != nil {
		return s.info.Priority
	}
	if o := p.orphans[common_info.PodGroupID(id)]; o != nil {
		return o.priority
	}
	return 0
}

func (p *plugin) ladderJobs() []ladder.Job {
	cfg := p.config.Ladder
	var jobs []ladder.Job
	for _, j := range p.tierJobs() {
		lj := ladder.Job{
			ID: j.ID, User: j.User, Units: j.Units, Running: j.Running, Created: j.Created, Started: j.Started,
		}
		if s := p.jobs[common_info.PodGroupID(j.ID)]; s != nil {
			labels := s.info.PodGroup.Labels
			if cfg.LevelLabel != "" {
				if level, err := strconv.ParseInt(strings.TrimSpace(labels[cfg.LevelLabel]), 10, 32); err == nil && level > 0 {
					lj.Level = int32(level)
				}
			}
			lj.Protect = cfg.ProtectLabel != "" && labels[cfg.ProtectLabel] == "true"
			lj.Lower = cfg.WhenFullLabel != "" && labels[cfg.WhenFullLabel] == "lower"
		}
		jobs = append(jobs, lj)
	}
	return jobs
}

// applyLadder marks protected jobs as not preemptible, in KAI's memory only.
func (p *plugin) applyLadder(out *ladderOutcome) {
	for id, r := range out.results {
		if s := p.jobs[common_info.PodGroupID(id)]; s != nil && r.NonPreemptible {
			s.info.Preemptibility = enginev2alpha2.NonPreemptible
		}
	}
	p.ladder = out
}

// ladderHold says why a laddered task may not be placed, or "" if it may.
func (p *plugin) ladderHold(job *jobState) string {
	if p.ladder == nil || job.running {
		return ""
	}
	r := p.ladder.results[string(job.info.UID)]
	if r.Tier != ladder.Held {
		return ""
	}
	return r.Reason
}

func (p *plugin) laddered(info *podgroup_info.PodGroupInfo) bool {
	s := p.jobs[info.UID]
	return s != nil && s.class == tiers.Laddered
}

// gapFilter lets a laddered job evict another only across evictGap.
func (p *plugin) gapFilter(preemptor, victim *podgroup_info.PodGroupInfo) (ok bool) {
	defer recoverTo("filtering victims", func() { ok = true })
	if !p.laddered(preemptor) || !p.laddered(victim) {
		return true
	}
	return preemptor.Priority-victim.Priority >= p.config.Ladder.EvictGap
}

// lastStartedFirst evicts, of laddered jobs of equal priority, the one that has lost
// the least, by its start.
func (p *plugin) lastStartedFirst(l, r any) (order int) {
	defer recoverTo("ordering victims", func() { order = 0 })
	lv, lok := l.(*podgroup_info.PodGroupInfo)
	rv, rok := r.(*podgroup_info.PodGroupInfo)
	if !lok || !rok || !p.laddered(lv) || !p.laddered(rv) ||
		lv.LastStartTimestamp == nil || rv.LastStartTimestamp == nil {
		return 0
	}
	switch {
	case lv.LastStartTimestamp.After(*rv.LastStartTimestamp):
		return -1
	case rv.LastStartTimestamp.After(*lv.LastStartTimestamp):
		return 1
	}
	return 0
}

// describe adds what the ladder knows of a job to its status.
func (o *ladderOutcome) describe(j *JobStatus, id string) {
	if o == nil {
		return
	}
	r := o.results[id]
	j.Ahead, j.Protected = r.Ahead.Float(), r.NonPreemptible
}

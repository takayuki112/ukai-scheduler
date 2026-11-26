// Package ladder decides each job's priority by how much its user runs: the more, the
// lower, step by step. It is a pure function of its inputs and does not depend on KAI.
package ladder

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// Tiers of the ladder, in addition to tiers.Unmanaged.
const (
	// Protected jobs may not be evicted.
	Protected tiers.Tier = "protected"
	// Level jobs have the level that their user chose.
	Level tiers.Tier = "level"
	// Step jobs have the step that their place in the line reaches.
	Step tiers.Tier = "step"
	// Held jobs may not start.
	Held tiers.Tier = "held"
)

// Rung is one step of the ladder.
type Rung struct {
	Priority int32
	// Width is ignored for the last rung, the floor.
	Width tiers.Units
}

// Policy holds the parameters. Limits apply to each user, except ProtectCluster.
type Policy struct {
	// Rungs are highest first. There is at least one.
	Rungs []Rung
	// ProtectCost multiplies the units of a protected job. At least 1.
	ProtectCost float64
	// ProtectLimit is how much protection a user may hold, at its cost. 0 allows none.
	ProtectLimit tiers.Units
	// ProtectCluster is how many units all protected jobs may hold together. 0 means
	// no limit.
	ProtectCluster tiers.Units
	// ProtectFor is how long protection lasts from the start. 0 means no end.
	ProtectFor time.Duration
}

// Job is one PodGroup of a class that the ladder manages.
type Job struct {
	ID    string
	User  string
	Units tiers.Units
	// Running is set while any pod holds or is releasing resources.
	Running bool
	Created time.Time
	Started time.Time // used only if Running
	// Level is the level that the user chose, or 0.
	Level int32
	// Protect asks that the job not be evicted.
	Protect bool
	// Lower lets a job whose level or protection does not fit run as it can, instead
	// of waiting.
	Lower bool
}

// Result is the outcome for one job.
type Result struct {
	tiers.Result
	// NonPreemptible is set for protected jobs.
	NonPreemptible bool
	// Ahead is what is ahead of the job in its user's line, protection at its cost.
	Ahead tiers.Units
}

// Assign returns the result for every job by ID. It does not modify jobs, and the
// order of jobs does not matter.
func Assign(p Policy, jobs []Job, now time.Time) map[string]Result {
	results := make(map[string]Result, len(jobs))
	users := map[string][]*Job{}
	for i := range jobs {
		j := &jobs[i]
		if j.Units <= 0 {
			results[j.ID] = Result{Result: tiers.Result{Tier: tiers.Unmanaged}}
			continue
		}
		users[j.User] = append(users[j.User], j)
	}
	protected := p.protect(users, now, results)
	for user, owned := range users {
		if user == "" {
			// Without a user, nothing is known of how much they run.
			for _, j := range owned {
				results[j.ID] = Result{Result: tiers.Result{Tier: Step, Priority: p.floor()}}
			}
			continue
		}
		p.assignUser(owned, protected, results)
	}
	return results
}

// protect decides which jobs are protected. Running ones stay protected until their
// time is up, even over a limit. Pending ones are admitted in order of submission, as
// both limits allow. Those not admitted are held, or with Lower, go on unprotected.
func (p Policy) protect(users map[string][]*Job, now time.Time, results map[string]Result) map[*Job]bool {
	protected := map[*Job]bool{}
	cost := map[string]tiers.Units{}
	var cluster tiers.Units
	var pending []*Job
	for user, owned := range users {
		for _, j := range owned {
			switch {
			case !j.Protect || user == "":
			case j.Running && p.ProtectFor > 0 && now.Sub(j.Started) >= p.ProtectFor:
			case j.Running:
				protected[j] = true
				cost[user] += p.cost(j)
				cluster += j.Units
			default:
				pending = append(pending, j)
			}
		}
	}
	slices.SortFunc(pending, byCreated)
	for _, j := range pending {
		reason := ""
		switch {
		case cost[j.User]+p.cost(j) > p.ProtectLimit:
			reason = fmt.Sprintf("protection: costs %s units, using %s of %s", p.cost(j), cost[j.User], p.ProtectLimit)
		case p.ProtectCluster > 0 && cluster+j.Units > p.ProtectCluster:
			reason = fmt.Sprintf("protection: needs %s units, %s of %s in the cluster protected", j.Units, cluster, p.ProtectCluster)
		}
		if reason == "" {
			protected[j] = true
			cost[j.User] += p.cost(j)
			cluster += j.Units
		} else if !j.Lower {
			results[j.ID] = Result{Result: tiers.Result{Tier: Held, Reason: reason}}
		}
	}
	return protected
}

func (p Policy) assignUser(jobs []*Job, protected map[*Job]bool, results map[string]Result) {
	var guarded, levels, rest []*Job
	for _, j := range jobs {
		switch {
		case results[j.ID].Tier == Held:
		case protected[j]:
			guarded = append(guarded, j)
		case j.Level > 0:
			levels = append(levels, j)
		default:
			rest = append(rest, j)
		}
	}
	runningFirst := func(a, b *Job) int {
		switch {
		case a.Running && !b.Running:
			return -1
		case b.Running && !a.Running:
			return 1
		case a.Running:
			return byStarted(a, b)
		}
		return byCreated(a, b)
	}
	slices.SortFunc(guarded, runningFirst)
	slices.SortFunc(levels, runningFirst)
	slices.SortFunc(rest, runningFirst)

	var ahead tiers.Units
	for _, j := range guarded {
		results[j.ID] = Result{
			Result: tiers.Result{Tier: Protected, Priority: p.Rungs[0].Priority}, NonPreemptible: true, Ahead: ahead,
		}
		ahead += p.cost(j)
	}

	// A level is a cap: what is protected and what has that level or higher must fit
	// in the rungs from it up. A running job keeps its level.
	type admitted struct {
		level int32
		units tiers.Units
	}
	var granted []admitted
	usedFrom := func(level int32) tiers.Units {
		used := ahead // protection counts against every level
		for _, g := range granted {
			if g.level >= level {
				used += g.units
			}
		}
		return used
	}
	var levelAhead tiers.Units
	for _, j := range levels {
		// A running job may have a level from before the ladder changed. One above the
		// top keeps the top, and a pending one waits for good.
		level := max(j.Level, p.floor())
		if j.Running {
			level = min(level, p.Rungs[0].Priority)
		} else {
			used := usedFrom(level)
			if limit := p.capFrom(level); used+j.Units > limit {
				if !j.Lower {
					results[j.ID] = Result{Result: tiers.Result{Tier: Held, Reason: fmt.Sprintf(
						"level %d: needs %s units, using %s of %s", level, j.Units, used, limit)}}
					continue
				}
				level = p.highestFitting(level, j.Units, usedFrom)
			}
		}
		granted = append(granted, admitted{level, j.Units})
		results[j.ID] = Result{Result: tiers.Result{Tier: Level, Priority: level}, Ahead: ahead + levelAhead}
		levelAhead += j.Units
	}

	ahead += levelAhead
	for _, j := range rest {
		ahead += j.Units
		results[j.ID] = Result{Result: tiers.Result{Tier: Step, Priority: p.at(ahead)}, Ahead: ahead - j.Units}
	}
}

// at is the priority of the rung where a job ends that takes the line up to total.
func (p Policy) at(total tiers.Units) int32 {
	var cum tiers.Units
	for _, r := range p.Rungs[:len(p.Rungs)-1] {
		cum += r.Width
		if total <= cum {
			return r.Priority
		}
	}
	return p.floor()
}

// capFrom is how much the rungs of at least the given level hold. The floor holds
// any amount.
func (p Policy) capFrom(level int32) tiers.Units {
	if level <= p.floor() {
		return math.MaxInt64 / 2
	}
	var cum tiers.Units
	for _, r := range p.Rungs[:len(p.Rungs)-1] {
		if r.Priority >= level {
			cum += r.Width
		}
	}
	return cum
}

// highestFitting is the highest rung below level where units fit.
func (p Policy) highestFitting(level int32, units tiers.Units, usedFrom func(int32) tiers.Units) int32 {
	for _, r := range p.Rungs {
		if r.Priority < level && usedFrom(r.Priority)+units <= p.capFrom(r.Priority) {
			return r.Priority
		}
	}
	return p.floor()
}

func (p Policy) floor() int32 { return p.Rungs[len(p.Rungs)-1].Priority }

func (p Policy) cost(j *Job) tiers.Units {
	return tiers.Units(math.Ceil(float64(j.Units) * max(p.ProtectCost, 1)))
}

func byCreated(a, b *Job) int {
	return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
}

func byStarted(a, b *Job) int {
	return cmp.Or(a.Started.Compare(b.Started), byCreated(a, b))
}

// Scale returns rungs whose widths are fractions of share.
func Scale(fractions []Rung, share tiers.Units) []Rung {
	out := slices.Clone(fractions)
	for i := range out {
		// Fractions are stored as units, so 0.25 is 250 thousandths.
		out[i].Width = tiers.Units(math.Floor(fractions[i].Width.Float() * float64(share)))
	}
	return out
}

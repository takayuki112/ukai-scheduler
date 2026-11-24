// Package tiers decides each job's effective tier. It is a pure function of its
// inputs and does not depend on KAI.
package tiers

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"
)

// Units is an amount of weighted GPU in thousandths, so that sums are exact.
type Units int64

const unitScale = 1000

// UnitsOf converts v, rounding up to the next thousandth.
func UnitsOf(v float64) Units {
	// The tolerance stops float noise, as in 0.1*3, from rounding up.
	return Units(math.Ceil(v*unitScale - 1e-6))
}

// Float returns the amount in whole units.
func (u Units) Float() float64 { return float64(u) / unitScale }

func (u Units) String() string {
	return strconv.FormatFloat(u.Float(), 'f', -1, 64)
}

// Class is how a job's priority class is treated.
type Class int

const (
	// Other jobs are ignored.
	Other Class = iota
	// Budgeted jobs use their owner's budget and are held when it is exhausted.
	Budgeted
	// Charged jobs use their owner's budget but are never held.
	Charged
	// Promotable jobs may be promoted into unused budget or demoted to excess.
	Promotable
)

// LargeJobRule says how to rank a promotable job larger than the whole allowance.
type LargeJobRule string

const (
	// LargeExcess always ranks it as excess.
	LargeExcess LargeJobRule = "excess"
	// LargeNormalIfFirst ranks it as normal if nothing of its owner's is ahead of
	// it in the allowance.
	LargeNormalIfFirst LargeJobRule = "normal-if-first"
)

// Policy holds the parameters of the rules. Limits apply to each owner.
type Policy struct {
	// Budget limits budgeted and charged jobs together.
	Budget Units
	// Allowance is how much promotable work ranks as normal.
	Allowance        Units
	PromotedPriority int32
	ExcessPriority   int32
	LargeJobs        LargeJobRule
}

// Job is one PodGroup.
type Job struct {
	ID       string
	Owner    string
	Class    Class
	Units    Units
	Priority int32 // before any change
	// Running is set while any pod holds or is releasing resources. The job is
	// then charged in full, once.
	Running bool
	Created time.Time
	Started time.Time // used only if Running
}

// Tier is a job's effective tier.
type Tier string

const (
	Unmanaged Tier = "unmanaged"
	Budget    Tier = "budget"
	Held      Tier = "held"
	Promoted  Tier = "promoted"
	Normal    Tier = "normal"
	Excess    Tier = "excess"
)

// Result is the outcome for one job.
type Result struct {
	Tier     Tier
	Priority int32
	// Reason explains a held job.
	Reason string
}

// Assign returns the result for every job by ID. It does not modify jobs, and the
// order of jobs does not matter.
func Assign(p Policy, jobs []Job) map[string]Result {
	results := make(map[string]Result, len(jobs))
	owners := map[string][]*Job{}
	for i := range jobs {
		j := &jobs[i]
		results[j.ID] = Result{Tier: Unmanaged, Priority: j.Priority}
		// Jobs without GPUs use no budget or allowance.
		if j.Class != Other && j.Units > 0 {
			owners[j.Owner] = append(owners[j.Owner], j)
		}
	}
	for owner, owned := range owners {
		if owner == "" {
			p.assignUnowned(owned, results)
		} else {
			p.assignOwner(owned, results)
		}
	}
	return results
}

// assignUnowned gives jobs without an owner no share of any budget or allowance.
func (p Policy) assignUnowned(jobs []*Job, results map[string]Result) {
	for _, j := range jobs {
		switch {
		case j.Class == Promotable:
			results[j.ID] = p.excess(j)
		case j.Class == Budgeted && !j.Running:
			results[j.ID] = Result{Tier: Held, Priority: j.Priority, Reason: "no user"}
		default:
			results[j.ID] = Result{Tier: Budget, Priority: j.Priority}
		}
	}
}

func (p Policy) assignOwner(jobs []*Job, results map[string]Result) {
	var used Units
	var waiting, running, pending []*Job
	for _, j := range jobs {
		switch {
		case j.Class == Promotable && j.Running:
			running = append(running, j)
		case j.Class == Promotable:
			pending = append(pending, j)
		case j.Class == Budgeted && !j.Running:
			waiting = append(waiting, j)
		default:
			// A pending charged job is not holding anything yet.
			if j.Running {
				used += j.Units
			}
			results[j.ID] = Result{Tier: Budget, Priority: j.Priority}
		}
	}
	slices.SortFunc(waiting, byCreated)
	slices.SortFunc(running, byStarted)
	slices.SortFunc(pending, byCreated)

	// Only what is running holds a waiting job: one that fits may start even if an
	// older one is waiting for GPUs. For promotion, also reserve for those that fit,
	// oldest first. One that does not fit blocks neither those behind it nor
	// promotion.
	var reserved Units
	for _, j := range waiting {
		if reason := p.OverBudget(j.Units, used); reason != "" {
			results[j.ID] = Result{Tier: Held, Priority: j.Priority, Reason: reason}
			continue
		}
		results[j.ID] = Result{Tier: Budget, Priority: j.Priority}
		if used+reserved+j.Units <= p.Budget {
			reserved += j.Units
		}
	}

	// Promote running jobs into what is left, longest running first. A job that has
	// just started then cannot displace a promoted one.
	spare := p.Budget - used - reserved
	var rest []*Job
	for _, j := range running {
		if j.Units <= spare {
			spare -= j.Units
			results[j.ID] = Result{Tier: Promoted, Priority: max(j.Priority, p.PromotedPriority)}
		} else {
			rest = append(rest, j)
		}
	}

	// The other running jobs and then the pending ones take up the allowance. A
	// pending job gets the priority it would have once placed, and both are in
	// creation order, not start order. Otherwise a job could evict another, drop to
	// excess, and be evicted back. A pending job never ranks as promoted.
	slices.SortFunc(rest, byCreated)
	var ahead Units
	for _, j := range rest {
		results[j.ID] = p.rank(j, ahead)
		ahead += j.Units
	}
	for _, j := range pending {
		results[j.ID] = p.rank(j, ahead)
		ahead += j.Units
	}
}

// rank places a job in the allowance behind the given units.
func (p Policy) rank(j *Job, ahead Units) Result {
	fits := ahead+j.Units <= p.Allowance
	if p.LargeJobs == LargeNormalIfFirst && j.Units > p.Allowance && ahead == 0 {
		fits = true
	}
	if fits {
		return Result{Tier: Normal, Priority: j.Priority}
	}
	return p.excess(j)
}

func (p Policy) excess(j *Job) Result {
	return Result{Tier: Excess, Priority: min(j.Priority, p.ExcessPriority)}
}

// OverBudget says why units more do not fit on top of used, or "" if they do.
func (p Policy) OverBudget(units, used Units) string {
	switch {
	case used+units <= p.Budget:
		return ""
	case units > p.Budget:
		return fmt.Sprintf("needs %s units, budget is %s", units, p.Budget)
	default:
		return fmt.Sprintf("needs %s units, using %s of %s", units, used, p.Budget)
	}
}

func byCreated(a, b *Job) int {
	return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
}

func byStarted(a, b *Job) int {
	return cmp.Or(a.Started.Compare(b.Started), byCreated(a, b))
}

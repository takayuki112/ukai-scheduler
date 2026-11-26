package tierbudget

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kai-scheduler/KAI-scheduler/pkg/common/constants"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/resource_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"

	"github.com/takayuki112/ukai-scheduler/pkg/ladder"
	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// Plugin arguments. Those without a default are required.
const (
	argMode             = "mode"              // audit (default) or enforce
	argUserLabel        = "userLabel"         // PodGroup label naming a job's user
	argBudgeted         = "budgetedClasses"   // comma-separated priority classes
	argCharged          = "chargedClasses"    // default: none
	argPromotable       = "promotableClasses" // default: none
	argBudget           = "budget"            // units per user
	argAllowance        = "allowance"         // units per user; required with promotableClasses
	argPromotedPriority = "promotedPriority"  // required with promotableClasses
	argExcessPriority   = "excessPriority"    // required with promotableClasses
	argLargeJobs        = "largeJobs"         // excess (default) or normal-if-first
	argGPUTypeLabel     = "gpuTypeLabel"      // nodeSelector key; default: none, so every GPU has the default weight
	argGPUWeights       = "gpuWeights"        // type=weight, comma-separated; needs gpuTypeLabel
	argConsolidation    = "consolidation"     // passes, see Pass; default: never move budgeted, charged or promoted jobs
	argProtected        = "protectedPriority" // without consolidation passes, running jobs from this priority up are not moved; default promotedPriority
	argJobMetrics       = "jobMetrics"        // false (default) or true: a metric for each job
	argDefaultWeight    = "defaultGPUWeight"  // units per GPU of any other type; default 1

	// The ladder scheme, see docs/ladder.md.
	argScheme         = "scheme"              // budget (default) or ladder
	argLadderClasses  = "ladderClasses"       // comma-separated priority classes; required with ladder
	argLadder         = "ladder"              // priority=width, ..., floor; required with ladder
	argLadderWidth    = "ladderWidth"         // units (default) or share
	argShareWindow    = "shareWindow"         // how long a user counts as active; default 1h
	argLevelLabel     = "levelLabel"          // PodGroup label with a chosen level; default: none
	argProtectLabel   = "protectLabel"        // PodGroup label, "true" to protect; default: none
	argWhenFullLabel  = "whenFullLabel"       // PodGroup label, "lower" to run lower instead of waiting
	argProtectCost    = "protectCost"         // default 3
	argProtectLimit   = "protectLimit"        // per user, at cost: units, or a fraction of the share
	argProtectCluster = "protectClusterLimit" // fraction of the cluster's units; default 0.5
	argProtectFor     = "protectFor"          // default 24h; 0 for no end
	argEvictGap       = "evictGap"            // default 0
)

var knownArgs = []string{
	argMode, argUserLabel, argBudgeted, argCharged, argPromotable, argBudget, argAllowance,
	argPromotedPriority, argExcessPriority, argLargeJobs, argGPUTypeLabel, argGPUWeights, argDefaultWeight,
	argConsolidation, argProtected, argJobMetrics,
	argScheme, argLadderClasses, argLadder, argLadderWidth, argShareWindow, argLevelLabel, argProtectLabel,
	argWhenFullLabel, argProtectCost, argProtectLimit, argProtectCluster, argProtectFor, argEvictGap,
}

// Scheme is how priorities are decided.
type Scheme string

const (
	// SchemeBudget is the budget, allowance and promotion of package tiers.
	SchemeBudget Scheme = "budget"
	// SchemeLadder is package ladder.
	SchemeLadder Scheme = "ladder"
)

// Mode says whether decisions take effect.
type Mode string

const (
	// Audit computes and reports decisions without applying them.
	Audit   Mode = "audit"
	Enforce Mode = "enforce"
)

// maxUnits keeps sums of units far from overflow.
const maxUnits = 1e9

// Config is the parsed plugin arguments.
type Config struct {
	Mode          Mode
	UserLabel     string
	Classes       map[string]tiers.Class // by priority class name
	Policy        tiers.Policy
	GPUTypeLabel  string
	GPUWeights    map[string]float64
	DefaultWeight float64
	// Passes are the consolidation passes. None means the default rule.
	Passes []Pass
	// ProtectedPriority is part of the default rule.
	ProtectedPriority int32
	// JobMetrics adds a metric for each job.
	JobMetrics bool

	Scheme Scheme
	Ladder LadderConfig
}

// LadderConfig is the configuration of the ladder scheme.
type LadderConfig struct {
	// Rungs have widths in units, or with Share, in fractions of the share.
	Rungs                                   []ladder.Rung
	Share                                   bool
	ShareWindow                             time.Duration
	LevelLabel, ProtectLabel, WhenFullLabel string
	ProtectCost                             float64
	// ProtectLimit is in units, or with Share, a fraction of the share.
	ProtectLimit   tiers.Units
	ProtectCluster float64 // fraction
	ProtectFor     time.Duration
	EvictGap       int32
}

// ParseConfig reads plugin arguments. The error lists every problem found.
func ParseConfig(args framework.PluginArguments) (*Config, error) {
	p := &parser{args: args}
	for _, k := range sortedKeys(args) {
		if !slices.Contains(knownArgs, k) {
			p.failf("%s: unknown argument", k)
		}
	}
	c := &Config{
		Mode:         Mode(p.oneOf(argMode, string(Audit), string(Enforce))),
		UserLabel:    p.required(argUserLabel),
		Classes:      map[string]tiers.Class{},
		GPUTypeLabel: p.get(argGPUTypeLabel),
		GPUWeights:   p.weights(),
		Passes:       p.passes(),
		JobMetrics:   p.oneOf(argJobMetrics, "false", "true") == "true",
	}
	c.DefaultWeight = 1
	if p.get(argDefaultWeight) != "" {
		c.DefaultWeight = p.number(argDefaultWeight)
	}

	c.Scheme = Scheme(p.oneOf(argScheme, string(SchemeBudget), string(SchemeLadder)))
	if c.GPUTypeLabel == "" && len(c.GPUWeights) > 0 {
		p.failf("%s needs %s", argGPUWeights, argGPUTypeLabel)
	}
	if c.Scheme == SchemeLadder {
		p.parseLadder(c)
		if err := errors.Join(p.errs...); err != nil {
			return nil, err
		}
		return c, nil
	}
	for _, k := range ladderOnly {
		if p.get(k) != "" {
			p.failf("%s: only with %s=%s", k, argScheme, SchemeLadder)
		}
	}

	p.required(argBudgeted)
	p.classes(c.Classes, argBudgeted, tiers.Budgeted)
	p.classes(c.Classes, argCharged, tiers.Charged)
	p.classes(c.Classes, argPromotable, tiers.Promotable)

	c.Policy.Budget = tiers.UnitsOf(p.number(argBudget))
	c.Policy.LargeJobs = tiers.LargeJobRule(p.oneOf(argLargeJobs, string(tiers.LargeExcess), string(tiers.LargeNormalIfFirst)))
	if p.get(argPromotable) != "" {
		c.Policy.Allowance = tiers.UnitsOf(p.number(argAllowance))
		c.Policy.PromotedPriority = p.priority(argPromotedPriority)
		c.Policy.ExcessPriority = p.priority(argExcessPriority)
		if len(p.errs) == 0 && c.Policy.ExcessPriority >= c.Policy.PromotedPriority {
			p.failf("%s must be below %s", argExcessPriority, argPromotedPriority)
		}
	}
	c.ProtectedPriority = c.Policy.PromotedPriority
	if p.get(argProtected) != "" || p.get(argPromotable) == "" && len(c.Passes) == 0 {
		c.ProtectedPriority = p.priority(argProtected)
	}
	if err := errors.Join(p.errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// Units weighs a number of GPUs by the type that a pod's nodeSelector asks for.
func (c *Config) Units(gpus float64, nodeSelector map[string]string) tiers.Units {
	weight := c.DefaultWeight
	if c.GPUTypeLabel != "" {
		if w, ok := c.GPUWeights[nodeSelector[c.GPUTypeLabel]]; ok {
			weight = w
		}
	}
	return tiers.UnitsOf(gpus * weight)
}

// TaskUnits is the charge for a pod. gpuMemory is that of each GPU of its node, in MiB,
// or 0 if it has no node or that is not known.
func (c *Config) TaskUnits(task *pod_info.PodInfo, gpuMemory int64) tiers.Units {
	gpus := task.GpuRequirement.GetGpusQuota()
	if task.GpuRequirement.GpuMemory() > 0 {
		// The share of a GPU depends on the node, so charge whole ones.
		gpus = float64(task.GpuRequirement.GetNumOfGpuDevices())
		if portion := boundPortion(task, gpuMemory); portion > 0 {
			gpus = resource_info.NewGpuResourceRequirementWithMultiFraction(task.GpuRequirement.GetNumOfGpuDevices(), portion, 0).GetGpusQuota()
		}
	}
	var selector map[string]string
	if task.Pod != nil {
		selector = task.Pod.Spec.NodeSelector
	}
	return c.Units(gpus, selector)
}

// boundPortion is the fraction that a pod asked for, if KAI's binder has since turned it
// into memory, and 0 if not. Users can set both annotations, and KAI goes by the memory,
// so the memory must not be more than the fraction of the node's GPUs.
func boundPortion(task *pod_info.PodInfo, gpuMemory int64) float64 {
	if gpuMemory <= 0 || task.Pod == nil {
		return 0
	}
	portion, err := strconv.ParseFloat(task.Pod.Annotations[constants.GpuFraction], 64)
	// KAI rounds shares to hundredths.
	if err != nil || !(portion > 0 && portion < 1) ||
		float64(task.GpuRequirement.GpuMemory())/float64(gpuMemory) > portion+0.01 {
		return 0
	}
	return portion
}

type parser struct {
	args framework.PluginArguments
	errs []error
}

func (p *parser) failf(format string, a ...any) { p.errs = append(p.errs, fmt.Errorf(format, a...)) }

func (p *parser) get(key string) string { return strings.TrimSpace(p.args[key]) }

func (p *parser) required(key string) string {
	v := p.get(key)
	if v == "" {
		p.failf("%s: required", key)
	}
	return v
}

// oneOf returns the argument, or the first choice if it is not set.
func (p *parser) oneOf(key string, choices ...string) string {
	v := p.get(key)
	if v == "" {
		return choices[0]
	}
	if !slices.Contains(choices, v) {
		p.failf("%s: %q is not one of %s", key, v, strings.Join(choices, ", "))
	}
	return v
}

func (p *parser) number(key string) float64 {
	v := p.required(key)
	if v == "" {
		return 0
	}
	return p.parseNumber(key, v)
}

func (p *parser) parseNumber(key, v string) float64 {
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || n < 0 || n > maxUnits {
		p.failf("%s: %q is not a number from 0 to %g", key, v, float64(maxUnits))
		return 0
	}
	return n
}

func (p *parser) priority(key string) int32 {
	v := p.required(key)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		p.failf("%s: %q is not a 32-bit integer", key, v)
	}
	return int32(n)
}

func (p *parser) list(key string) []string {
	if p.get(key) == "" {
		return nil
	}
	items := strings.Split(p.get(key), ",")
	for i := range items {
		items[i] = strings.TrimSpace(items[i])
		if items[i] == "" {
			p.failf("%s: empty item", key)
		}
	}
	return items
}

func (p *parser) classes(into map[string]tiers.Class, key string, class tiers.Class) {
	for _, name := range p.list(key) {
		if _, dup := into[name]; dup {
			p.failf("%s: %q is listed more than once", key, name)
		}
		into[name] = class
	}
}

func (p *parser) weights() map[string]float64 {
	weights := map[string]float64{}
	for _, item := range p.list(argGPUWeights) {
		name, value, ok := strings.Cut(item, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" {
			p.failf("%s: %q is not type=weight", argGPUWeights, item)
			continue
		}
		if _, dup := weights[name]; dup {
			p.failf("%s: %q is listed more than once", argGPUWeights, name)
		}
		weights[name] = p.parseNumber(argGPUWeights, value)
	}
	return weights
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

var (
	budgetOnly = []string{argBudgeted, argCharged, argPromotable, argBudget, argAllowance,
		argPromotedPriority, argExcessPriority, argLargeJobs}
	ladderOnly = []string{argLadderClasses, argLadder, argLadderWidth, argShareWindow, argLevelLabel,
		argProtectLabel, argWhenFullLabel, argProtectCost, argProtectLimit, argProtectCluster, argProtectFor,
		argEvictGap}
)

func (p *parser) parseLadder(c *Config) {
	for _, k := range budgetOnly {
		if p.get(k) != "" {
			p.failf("%s: not with %s=%s", k, argScheme, SchemeLadder)
		}
	}
	p.required(argLadderClasses)
	p.classes(c.Classes, argLadderClasses, tiers.Laddered)
	l := &c.Ladder
	l.Share = p.oneOf(argLadderWidth, "units", "share") == "share"
	l.Rungs = p.rungs()
	l.ShareWindow = p.duration(argShareWindow, time.Hour)
	l.LevelLabel, l.ProtectLabel, l.WhenFullLabel = p.get(argLevelLabel), p.get(argProtectLabel), p.get(argWhenFullLabel)
	l.ProtectCost = p.optionalNumber(argProtectCost, 3)
	if l.ProtectCost < 1 {
		p.failf("%s: must be at least 1", argProtectCost)
	}
	l.ProtectLimit = tiers.UnitsOf(p.optionalNumber(argProtectLimit, 0))
	l.ProtectCluster = p.optionalNumber(argProtectCluster, 0.5)
	if l.ProtectCluster > 1 {
		p.failf("%s: a fraction from 0 to 1", argProtectCluster)
	}
	l.ProtectFor = p.duration(argProtectFor, 24*time.Hour)
	if p.get(argEvictGap) != "" {
		l.EvictGap = p.priority(argEvictGap)
	}
	if l.ProtectLabel != "" && l.ProtectLimit == 0 {
		p.failf("%s needs %s", argProtectLabel, argProtectLimit)
	}
	// Consolidation never moves running jobs on the top rung, unless set otherwise.
	c.ProtectedPriority = math.MaxInt32
	if len(l.Rungs) > 0 {
		c.ProtectedPriority = l.Rungs[0].Priority
	}
	if p.get(argProtected) != "" {
		c.ProtectedPriority = p.priority(argProtected)
	}
}

// rungs parses "75=4, 70=4, 40": priorities from high to low, the last without a width.
func (p *parser) rungs() []ladder.Rung {
	items := p.list(argLadder)
	if len(items) == 0 {
		p.failf("%s: required", argLadder)
		return nil
	}
	var rungs []ladder.Rung
	for i, item := range items {
		priority, width, hasWidth := strings.Cut(item, "=")
		last := i == len(items)-1
		if hasWidth == last {
			p.failf("%s: %q: every step but the last needs a width, as priority=width", argLadder, item)
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(priority), 10, 32)
		if err != nil {
			p.failf("%s: %q is not a 32-bit integer", argLadder, priority)
			continue
		}
		r := ladder.Rung{Priority: int32(n)}
		if !last {
			w := p.parseNumber(argLadder, strings.TrimSpace(width))
			if w <= 0 {
				p.failf("%s: %q: the width must be above 0", argLadder, item)
			}
			r.Width = tiers.UnitsOf(w)
		}
		if len(rungs) > 0 && r.Priority >= rungs[len(rungs)-1].Priority {
			p.failf("%s: priorities must fall from step to step", argLadder)
		}
		rungs = append(rungs, r)
	}
	return rungs
}

func (p *parser) optionalNumber(key string, otherwise float64) float64 {
	if p.get(key) == "" {
		return otherwise
	}
	return p.parseNumber(key, p.get(key))
}

func (p *parser) duration(key string, otherwise time.Duration) time.Duration {
	if p.get(key) == "" {
		return otherwise
	}
	d, err := time.ParseDuration(p.get(key))
	if err != nil || d < 0 {
		p.failf("%s: %q is not a duration such as 30m", key, p.get(key))
	}
	return d
}

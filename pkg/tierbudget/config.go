package tierbudget

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// Plugin arguments. Those without a default are required.
const (
	argMode             = "mode"              // audit (default) or enforce
	argOwnerLabel       = "ownerLabel"        // PodGroup label naming a job's owner
	argBudgeted         = "budgetedClasses"   // comma-separated priority classes
	argCharged          = "chargedClasses"    // default: none
	argPromotable       = "promotableClasses" // default: none
	argBudget           = "budget"            // units per owner
	argAllowance        = "allowance"         // units per owner; required with promotableClasses
	argPromotedPriority = "promotedPriority"  // required with promotableClasses
	argExcessPriority   = "excessPriority"    // required with promotableClasses
	argLargeJobs        = "largeJobs"         // excess (default) or normal-if-first
	argGPUTypeLabel     = "gpuTypeLabel"      // nodeSelector key; default: none, so every GPU has the default weight
	argGPUWeights       = "gpuWeights"        // type=weight, comma-separated; needs gpuTypeLabel
	argDefaultWeight    = "defaultGPUWeight"  // units per GPU of any other type; default 1
)

var knownArgs = []string{
	argMode, argOwnerLabel, argBudgeted, argCharged, argPromotable, argBudget, argAllowance,
	argPromotedPriority, argExcessPriority, argLargeJobs, argGPUTypeLabel, argGPUWeights, argDefaultWeight,
}

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
	OwnerLabel    string
	Classes       map[string]tiers.Class // by priority class name
	Policy        tiers.Policy
	GPUTypeLabel  string
	GPUWeights    map[string]float64
	DefaultWeight float64
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
		OwnerLabel:   p.required(argOwnerLabel),
		Classes:      map[string]tiers.Class{},
		GPUTypeLabel: p.get(argGPUTypeLabel),
		GPUWeights:   p.weights(),
	}
	c.DefaultWeight = 1
	if p.get(argDefaultWeight) != "" {
		c.DefaultWeight = p.number(argDefaultWeight)
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
	if c.GPUTypeLabel == "" && len(c.GPUWeights) > 0 {
		p.failf("%s needs %s", argGPUWeights, argGPUTypeLabel)
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

package tierbudget

import (
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

func validArgs() framework.PluginArguments {
	return framework.PluginArguments{
		"mode": "enforce", "userLabel": "example.com/user",
		"budgetedClasses": "high", "chargedClasses": "workspace", "promotableClasses": "mid, mid-alt",
		"budget": "4", "allowance": "4.5", "promotedPriority": "75", "excessPriority": "45", "largeJobs": "normal-if-first",
		"gpuTypeLabel": "gpu-type", "gpuWeights": "small=1, big = 2.5", "defaultGPUWeight": "4",
		"consolidation": "high>high, low; mid,low > mid,@promoted",
	}
}

func TestParseConfig(t *testing.T) {
	got, err := ParseConfig(validArgs())
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Mode: Enforce, UserLabel: "example.com/user", Scheme: SchemeBudget,
		Classes:      map[string]tiers.Class{"high": tiers.Budgeted, "workspace": tiers.Charged, "mid": tiers.Promotable, "mid-alt": tiers.Promotable},
		Policy:       tiers.Policy{Budget: 4000, Allowance: 4500, PromotedPriority: 75, ExcessPriority: 45, LargeJobs: tiers.LargeNormalIfFirst},
		GPUTypeLabel: "gpu-type", GPUWeights: map[string]float64{"small": 1, "big": 2.5}, DefaultWeight: 4, ProtectedPriority: 75,
		Passes: []Pass{{Place: []string{"high"}, Move: []string{"high", "low"}}, {Place: []string{"mid", "low"}, Move: []string{"mid", "@promoted"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	c, err := ParseConfig(framework.PluginArguments{"userLabel": "o", "budgetedClasses": "high", "budget": "4", "protectedPriority": "75"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != Audit || c.Policy.LargeJobs != tiers.LargeExcess || c.DefaultWeight != 1 {
		t.Errorf("got %+v", c)
	}
}

func TestParseConfigErrors(t *testing.T) {
	for _, tc := range []struct{ key, value, want string }{
		{"mode", "on", "mode: "},
		{"userLabel", "", "userLabel: required"},
		{"budgetedClasses", " ", "budgetedClasses: required"},
		{"budgetedClasses", "high,,x", "empty item"},
		{"chargedClasses", "high", "more than once"},
		{"budget", "", "budget: required"},
		{"budget", "-1", "budget: "},
		{"budget", "NaN", "budget: "},
		{"budget", "Inf", "budget: "},
		{"budget", "four", "budget: "},
		{"allowance", "", "allowance: required"},
		{"promotedPriority", "", "promotedPriority: required"},
		{"promotedPriority", "99999999999", "promotedPriority: "},
		{"excessPriority", "75", "must be below"},
		{"largeJobs", "never", "largeJobs: "},
		{"gpuTypeLabel", "", "needs gpuTypeLabel"},
		{"gpuWeights", "small", "not type=weight"},
		{"gpuWeights", "small=x", "gpuWeights: "},
		{"gpuWeights", "small=1,small=2", "more than once"},
		{"defaultGPUWeight", "-4", "defaultGPUWeight: "},
		{"consolidation", "high", "not place>move"},
		{"consolidation", "high>", "empty item"},
		{"consolidation", ">low", "empty item"},
		{"consolidation", "high>low;", "not place>move"},
		{"consolidation", "high>low,low", "more than once"},
		{"consolidation", "high>@normal", "unknown group"},
		{"protectedPriority", "x", "protectedPriority: "},
		{"budgett", "4", "budgett: unknown"},
		{"ownerLabel", "x", "ownerLabel: unknown"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			args := maps.Clone(validArgs())
			args[tc.key] = tc.value
			c, err := ParseConfig(args)
			if err == nil || c != nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, %v; want error with %q", c, err, tc.want)
			}
		})
	}
}

func TestProtectedPriority(t *testing.T) {
	args := validArgs()
	args["protectedPriority"] = "60"
	if c, err := ParseConfig(args); err != nil || c.ProtectedPriority != 60 {
		t.Errorf("got %v, %v", c, err)
	}
	// Nothing to default to, and no passes that say what may move.
	_, err := ParseConfig(framework.PluginArguments{"userLabel": "o", "budgetedClasses": "high", "budget": "4"})
	if err == nil || !strings.Contains(err.Error(), "protectedPriority: required") {
		t.Errorf("got %v", err)
	}
}

func TestParseConfigReportsAllErrors(t *testing.T) {
	_, err := ParseConfig(framework.PluginArguments{"mode": "x", "zzz": "1"})
	for _, want := range []string{"mode", "zzz", "userLabel", "budgetedClasses", "budget"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v lacks %q", err, want)
		}
	}
}

func TestUnits(t *testing.T) {
	c, err := ParseConfig(validArgs())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		gpus     float64
		selector map[string]string
		want     tiers.Units
	}{
		{2, map[string]string{"gpu-type": "small"}, 2000},
		{0.5, map[string]string{"gpu-type": "big"}, 1250},
		{1, map[string]string{"gpu-type": "other"}, 4000},
		{1, nil, 4000},
		{0, nil, 0},
	} {
		if got := c.Units(tc.gpus, tc.selector); got != tc.want {
			t.Errorf("%v %v: got %d, want %d", tc.gpus, tc.selector, got, tc.want)
		}
	}
}

func FuzzParseConfig(f *testing.F) {
	f.Add("4", "a=1,b=2", "x,y", "75")
	f.Fuzz(func(t *testing.T, budget, weights, classes, priority string) {
		args := validArgs()
		args["budget"], args["gpuWeights"], args["promotableClasses"], args["promotedPriority"] = budget, weights, classes, priority
		if c, err := ParseConfig(args); err == nil && c.Policy.Budget < 0 {
			t.Errorf("negative budget from %q", budget)
		}
	})
}

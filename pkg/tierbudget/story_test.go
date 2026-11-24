package tierbudget

import (
	"testing"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// The example in the README and in docs/images/story.gif: 8 GPUs, a budget of 4 and an
// allowance of 2. Keep them in step.
func TestDocumentedStory(t *testing.T) {
	args := testArgs("4")
	args["allowance"] = "2"
	for _, step := range []struct {
		name  string
		jobs  []testJob
		state map[string]string
		tier  map[string]tiers.Tier
	}{
		{
			"alice's P2 starts", []testJob{j("train", "alice", "mid", 4, pending, 9)},
			map[string]string{"train": "placed"}, map[string]tiers.Tier{"train": tiers.Excess},
		},
		{
			"it is promoted, and bob's P1 starts", []testJob{j("train", "alice", "mid", 4, running, 9), j("eval", "bob", "high", 4, pending, 8)},
			map[string]string{"train": "running", "eval": "placed"}, map[string]tiers.Tier{"train": tiers.Promoted, "eval": tiers.Budget},
		},
		{
			"bob's second P1 is over budget", []testJob{j("train", "alice", "mid", 4, running, 9), j("eval", "bob", "high", 4, running, 8), j("eval2", "bob", "high", 2, pending, 7)},
			map[string]string{"train": "running", "eval": "running", "eval2": "pending"}, map[string]tiers.Tier{"train": tiers.Promoted, "eval2": tiers.Held},
		},
		{
			"alice's own P1 takes the place of her P2",
			[]testJob{j("train", "alice", "mid", 4, running, 9), j("eval", "bob", "high", 4, running, 8), j("eval2", "bob", "high", 2, pending, 7), j("debug", "alice", "high", 4, pending, 6)},
			map[string]string{"train": "evicted", "eval": "running", "eval2": "pending", "debug": "placed"},
			map[string]tiers.Tier{"train": tiers.Excess, "eval2": tiers.Held, "debug": tiers.Budget},
		},
		{
			"bob's first P1 ends, so his second starts",
			[]testJob{j("train", "alice", "mid", 4, pending, 9), j("eval2", "bob", "high", 2, pending, 7), j("debug", "alice", "high", 4, running, 6)},
			map[string]string{"train": "pending", "eval2": "placed", "debug": "running"}, map[string]tiers.Tier{"train": tiers.Excess},
		},
		{
			"that ends too, so alice's P2 resumes on what is free",
			[]testJob{j("train", "alice", "mid", 4, pending, 9), j("debug", "alice", "high", 4, running, 6)},
			map[string]string{"train": "placed", "debug": "running"}, map[string]tiers.Tier{"train": tiers.Excess},
		},
	} {
		t.Run(step.name, func(t *testing.T) {
			ssn := session(t, map[string]int{"n0": 8}, step.jobs)
			schedule(ssn, args)
			for name, want := range step.state {
				if got := state(find(t, ssn, name)); got != want {
					t.Errorf("%s: %s, want %s", name, got, want)
				}
			}
			for _, s := range LastStatus().Jobs {
				if want, ok := step.tier[s.Name]; ok && s.Tier != want {
					t.Errorf("%s: tier %s, want %s", s.Name, s.Tier, want)
				}
			}
		})
	}
}

// Command replay shows what the plugin would do to a KAI snapshot, next to stock KAI.
// It runs offline and changes nothing.
package main

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"text/tabwriter"

	"sigs.k8s.io/yaml"

	"github.com/takayuki112/ukai-scheduler/pkg/replay"
	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

func main() {
	snapshotPath := flag.String("snapshot", "", "zip from KAI's /get-snapshot")
	argsPath := flag.String("arguments", "", "YAML map of the plugin's arguments")
	all := flag.Bool("all", false, "list every managed job, not only those that are affected")
	flag.Parse()
	if *snapshotPath == "" || *argsPath == "" || flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(os.Stdout, *snapshotPath, *argsPath, *all); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(w io.Writer, snapshotPath, argsPath string, all bool) error {
	text, err := os.ReadFile(argsPath)
	if err != nil {
		return err
	}
	var args map[string]string
	if err := yaml.UnmarshalStrict(text, &args); err != nil {
		return fmt.Errorf("%s: all values must be strings: %w", argsPath, err)
	}
	s, err := replay.Load(snapshotPath)
	if err != nil {
		return err
	}
	stock, err := replay.Stock(s)
	if err != nil {
		return err
	}
	with, err := replay.With(s, args)
	if err != nil {
		return err
	}

	tab := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tab, "JOB\tOWNER\tCLASS\tUNITS\tSTATE\tTIER\tPRIORITY\tSTOCK\tWITH PLUGIN\tREASON")
	seen := map[string]bool{}
	counts := map[tiers.Tier]int{}
	for _, j := range with.Status.Jobs {
		name := j.Namespace + "/" + j.Name
		seen[name] = true
		counts[j.Tier]++
		a, b := stock.Actions[name], with.Actions[name]
		if !all && a == b && j.Tier != tiers.Held && j.Priority == j.ClassPriority {
			continue
		}
		state := "pending"
		if j.Running {
			state = "running"
		}
		priority := fmt.Sprint(j.Priority)
		if j.Priority != j.ClassPriority {
			priority = fmt.Sprintf("%d (was %d)", j.Priority, j.ClassPriority)
		}
		fmt.Fprintf(tab, "%s\t%s\t%s\t%g\t%s\t%s\t%s\t%s\t%s\t%s\n", name, cmp.Or(j.Owner, "-"), j.Class, j.Units,
			state, j.Tier, priority, show(a), show(b), j.Reason)
	}
	// Other jobs, if the plugin changes what happens to them.
	var others []string
	for _, actions := range []map[string]replay.Action{stock.Actions, with.Actions} {
		for name := range actions {
			if !seen[name] && stock.Actions[name] != with.Actions[name] {
				seen[name] = true
				others = append(others, name)
			}
		}
	}
	slices.Sort(others)
	for _, name := range others {
		fmt.Fprintf(tab, "%s\t-\t-\t-\t-\t-\t-\t%s\t%s\t\n", name, show(stock.Actions[name]), show(with.Actions[name]))
	}
	if err := tab.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "\n%d managed jobs:", len(with.Status.Jobs))
	for _, t := range []tiers.Tier{tiers.Budget, tiers.Held, tiers.Promoted, tiers.Normal, tiers.Excess} {
		fmt.Fprintf(w, " %d %s", counts[t], t)
	}
	fmt.Fprintln(w)
	for _, r := range []struct {
		name   string
		result *replay.Result
	}{{"stock", stock}, {"with plugin", with}} {
		n := map[replay.Action]int{}
		for _, a := range r.result.Actions {
			n[a]++
		}
		fmt.Fprintf(w, "%s: %d placed, %d evicted, %d moved\n", r.name, n[replay.Placed], n[replay.Evicted], n[replay.Moved])
	}
	return nil
}

func show(a replay.Action) string { return cmp.Or(string(a), "-") }

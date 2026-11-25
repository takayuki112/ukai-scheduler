package dashboard

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"

	kaifake "github.com/kai-scheduler/KAI-scheduler/pkg/apis/client/clientset/versioned/fake"
	enginev2alpha2 "github.com/kai-scheduler/KAI-scheduler/pkg/apis/scheduling/v2alpha2"

	"github.com/takayuki112/ukai-scheduler/pkg/tierbudget"
	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func config(t *testing.T, change ...string) *tierbudget.Config {
	t.Helper()
	args := map[string]string{
		"mode": "enforce", "userLabel": "example.com/user", "budgetedClasses": "high", "chargedClasses": "workspace", "promotableClasses": "mid",
		"budget": "4", "allowance": "4", "promotedPriority": "75", "excessPriority": "45",
		"gpuTypeLabel": "gpu-type", "gpuWeights": "small=1,big=2", "defaultGPUWeight": "2",
	}
	for i := 0; i < len(change); i += 2 {
		args[change[i]] = change[i+1]
	}
	c, err := tierbudget.ParseConfig(args)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type jobOption func(*enginev2alpha2.PodGroup, *v1.Pod)

// job makes a PodGroup with one pod, created some minutes ago.
func job(name, user, class, gpuType string, gpus, minutes int, options ...jobOption) (*enginev2alpha2.PodGroup, *v1.Pod) {
	created := metav1.NewTime(now.Add(-time.Duration(minutes) * time.Minute))
	labels := map[string]string{"example.com/user": user}
	g := &enginev2alpha2.PodGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: name, Labels: labels, CreationTimestamp: created},
		Spec:       enginev2alpha2.PodGroupSpec{PriorityClassName: class, Queue: "q"},
	}
	p := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: name + "-0", UID: "u", Labels: labels, CreationTimestamp: created, Annotations: map[string]string{"pod-group-name": name}},
		Spec: v1.PodSpec{PriorityClassName: class, Containers: []v1.Container{{
			Name: "main", Env: []v1.EnvVar{{Name: "SECRET", Value: "x"}}, Command: []string{"x"},
		}}},
		Status: v1.PodStatus{Phase: v1.PodPending},
	}
	if gpus > 0 {
		l := v1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(int64(gpus), resource.DecimalSI)}
		p.Spec.Containers[0].Resources = v1.ResourceRequirements{Limits: l, Requests: l}
	}
	if gpuType != "" {
		p.Spec.NodeSelector = map[string]string{"gpu-type": gpuType}
	}
	for _, o := range options {
		o(g, p)
	}
	return g, p
}

func runs(minutes int) jobOption {
	return func(g *enginev2alpha2.PodGroup, p *v1.Pod) {
		p.Spec.NodeName, p.Status.Phase = "n0", v1.PodRunning
		g.Annotations = map[string]string{"kai.scheduler/last-start-timestamp": now.Add(-time.Duration(minutes) * time.Minute).Format(time.RFC3339)}
	}
}

func annotate(kv ...string) jobOption {
	return func(_ *enginev2alpha2.PodGroup, p *v1.Pod) {
		for i := 0; i < len(kv); i += 2 {
			p.Annotations[kv[i]] = kv[i+1]
		}
	}
}

func says(message string) jobOption {
	return func(g *enginev2alpha2.PodGroup, _ *v1.Pod) {
		g.Status.SchedulingConditions = []enginev2alpha2.SchedulingCondition{{Status: v1.ConditionTrue, Message: message}}
	}
}

func (d *Data) with(g *enginev2alpha2.PodGroup, p *v1.Pod) {
	d.Groups, d.Pods = append(d.Groups, g), append(d.Pods, p)
}

func data() Data {
	return Data{SeesPods: func(string) bool { return true }, GPUMemory: map[string]int64{"n0": 10000}}
}

func sample() Data {
	d := data()
	d.with(job("pg-eval", "alice", "high", "small", 2, 50, runs(40)))
	d.with(job("pg-train", "alice", "mid", "small", 2, 300, runs(290)))
	d.with(job("pg-sweep", "alice", "mid", "big", 1, 200, runs(100)))
	d.with(job("pg-long", "alice", "mid", "big", 2, 90))
	d.with(job("pg-next", "alice", "high", "big", 2, 20, says("1 node(s): user budget: needs 4 units, using 2 of 4. More")))
	d.with(job("pg-fit", "bob", "high", "big", 1, 30))
	d.with(job("pg-wide", "bob", "high", "big", 4, 25))
	d.with(job("pg-other", "carol", "high", "small", 1, 10))
	return d
}

func draw(t *testing.T, v *View, width int) string {
	return strings.Join(Render(v, Options{Width: width, Others: true, Limit: 10, Now: now}), "\n") + "\n"
}

// The sample in the docs is what is drawn.
func TestDocumentedScreen(t *testing.T) {
	got := draw(t, Compute(config(t), "alice", sample()), 100)
	doc, err := os.ReadFile("../../docs/dashboard.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "```text\n"+got+"```") {
		t.Errorf("docs/dashboard.md does not show:\n%s", got)
	}
}

func find(t *testing.T, v *View, name string) *Job {
	t.Helper()
	for _, j := range v.Jobs {
		if j.ID == "team/"+name {
			return j
		}
	}
	t.Fatalf("no %s", name)
	return nil
}

func TestTiers(t *testing.T) {
	v := Compute(config(t), "alice", sample())
	for name, want := range map[string]tiers.Tier{
		"pg-eval": tiers.Budget, "pg-train": tiers.Promoted, "pg-sweep": tiers.Normal, "pg-long": tiers.Excess, "pg-next": tiers.Held,
	} {
		if got := find(t, v, name).Tier; got != want {
			t.Errorf("%s is %s, want %s", name, got, want)
		}
	}
	if v.Used != 2000 || find(t, v, "pg-long").Units != 4000 {
		t.Errorf("used %v, units %v", v.Used, find(t, v, "pg-long").Units)
	}
	if len(v.Queues) != 2 || v.Queues[0].Type != "big" {
		t.Fatalf("queues %+v", v.Queues)
	}
	var order []string
	for _, j := range v.Queues[0].Jobs {
		order = append(order, j.ID)
	}
	if want := []string{"team/pg-fit", "team/pg-wide", "team/pg-next"}; !slices.Equal(order, want) {
		t.Errorf("order %v, want %v", order, want)
	}
	if v.Queues[0].Jobs[1].Tier != tiers.Held {
		t.Error("bob's job over the budget should be held")
	}
}

func TestSchedulersReason(t *testing.T) {
	for name, tc := range map[string]struct {
		message string
		tier    tiers.Tier
		reason  string
	}{
		"held":        {"x user budget: needs 0.5 units, using 3.5 of 4.", tiers.Held, "needs 0.5 units, using 3.5 of 4"},
		"too large":   {"user budget: needs 8 units, budget is 4", tiers.Held, "needs 8 units, budget is 4"},
		"other":       {"no nodes with enough GPUs", tiers.Budget, ""},
		"not exactly": {"user budget: \x1b[2J needs you", tiers.Budget, ""},
	} {
		t.Run(name, func(t *testing.T) {
			d := data()
			d.with(job("a", "alice", "high", "small", 1, 5, says(tc.message)))
			if j := find(t, Compute(config(t), "alice", d), "a"); j.Tier != tc.tier || j.Reason != tc.reason {
				t.Errorf("%s, %q", j.Tier, j.Reason)
			}
			d = data() // left over from before it started
			d.with(job("a", "alice", "high", "small", 1, 5, says(tc.message), runs(1)))
			if j := find(t, Compute(config(t), "alice", d), "a"); j.Tier != tiers.Budget || j.Reason != "" {
				t.Errorf("running: %s, %q", j.Tier, j.Reason)
			}
		})
	}
}

// These are the cases of the plugin's TestBoundFractions, from the pod on.
func TestUnits(t *testing.T) {
	const request = "nvidia.com/container.main.gpu-memory.request"
	for name, tc := range map[string]struct {
		gpus    int
		options []jobOption
		memory  int64
		want    tiers.Units
	}{
		"whole":                    {2, nil, 0, 2000},
		"waiting fraction":         {0, []jobOption{annotate("gpu-fraction", "0.5")}, 0, 500},
		"several":                  {0, []jobOption{annotate("gpu-fraction", "0.75", "gpu-fraction-num-devices", "2")}, 0, 1500},
		"bound fraction":           {0, []jobOption{annotate("gpu-fraction", "0.5", request, "5000Mi"), runs(1)}, 10000, 500},
		"more memory than it says": {0, []jobOption{annotate("gpu-fraction", "0.01", request, "9000Mi"), runs(1)}, 10000, 1000},
		"unknown GPU memory":       {0, []jobOption{annotate("gpu-fraction", "0.5", request, "5000Mi"), runs(1)}, 0, 1000},
		"memory":                   {0, []jobOption{annotate("gpu-memory", "2000")}, 0, 1000},
	} {
		t.Run(name, func(t *testing.T) {
			d := data()
			d.GPUMemory["n0"] = tc.memory
			g, p := job("a", "alice", "high", "small", tc.gpus, 5, tc.options...)
			stripped, _ := stripPod(p)
			d.with(g, stripped.(*v1.Pod))
			if got := find(t, Compute(config(t), "alice", d), "a").Units; got != tc.want {
				t.Errorf("%v units, want %v", got, tc.want)
			}
		})
	}
}

func TestRemains(t *testing.T) {
	d := data()
	_, p := job("a", "alice", "high", "small", 3, 5, runs(4))
	p.DeletionTimestamp = &metav1.Time{Time: now}
	d.Pods = []*v1.Pod{p}
	p.Labels = nil // only the PodGroup had it
	if v := Compute(config(t), "alice", d); v.Used != 0 {
		t.Errorf("used %v", v.Used)
	}
	d.Former = map[string]Group{"team/a": {"alice", "high"}}
	if v := Compute(config(t), "alice", d); v.Used != 3000 || !find(t, v, "a").Gone {
		t.Errorf("used %v", v.Used)
	}
}

func TestWithoutPods(t *testing.T) {
	d := data()
	d.SeesPods = func(string) bool { return false }
	for _, user := range []string{"alice", "bob"} {
		g, _ := job("pg-"+user, user, "high", "small", 1, 5)
		g.Status.ResourcesStatus.Requested = v1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
		d.Groups = append(d.Groups, g)
	}
	v := Compute(config(t), "alice", d)
	if j := find(t, v, "pg-alice"); !j.Estimated || j.Units != 2000 || j.Running {
		t.Errorf("%+v", j)
	}
	text := draw(t, v, 0)
	for _, want := range []string{"~2", "add -n team", "GPUs of a type that is not visible: you are number 1 of 2"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
}

func TestScreenIsSafe(t *testing.T) {
	d := data()
	d.with(job("a\x1b[2Jb\r\n‮", "alice", "high", "small\x07", 1, 5))
	v := Compute(config(t), "alice", d)
	v.Lost, v.Notes = "\x1b]0;x\x07", []string{"\x9b2J"}
	for _, color := range []bool{false, true} {
		for _, l := range Render(v, Options{Width: 80, Color: color, Others: true, Limit: 1, Now: now}) {
			for _, code := range []style{bold, dim, red, green, yellow, blue, purple} {
				l = strings.TrimPrefix(l, "\x1b["+string(code)+"m")
			}
			l = strings.TrimSuffix(l, "\x1b[0m")
			if strings.ContainsFunc(l, func(r rune) bool { return r < ' ' || r > '~' }) {
				t.Errorf("%q", l)
			}
		}
	}
}

func TestWidths(t *testing.T) {
	v := Compute(config(t, "mode", "audit"), "alice", sample())
	for _, width := range []int{1, 20, 40, 80, 159, 160, 200} {
		for _, l := range Render(v, Options{Width: width, Others: true, Limit: 1, Now: now}) {
			if len(l) > width {
				t.Errorf("width %d: %q", width, l)
			}
		}
	}
	text := draw(t, v, 0)
	if !strings.Contains(text, "AUDIT") {
		t.Error("audit is not shown")
	}
	hidden := strings.Join(Render(v, Options{Limit: 1, Now: now}), "\n")
	if strings.Contains(hidden, "bob") || strings.Contains(hidden, "pg-fit") || !strings.Contains(hidden, "and 1 more of others") || !strings.Contains(hidden, "team/pg-next") {
		t.Errorf("anonymous:\n%s", hidden)
	}
}

func TestWideScreen(t *testing.T) {
	text := draw(t, Compute(config(t), "alice", sample()), 160)
	if !regexp.MustCompile(`(?m)^  JOB .* P1 waiting`).MatchString(text) {
		t.Errorf("not side by side:\n%s", text)
	}
}

func TestFrame(t *testing.T) {
	lines := []string{"a", "b", "c", "d"}
	for _, tc := range []struct{ top, height, want int }{{0, 10, 0}, {9, 3, 2}, {-1, 3, 0}, {1, 3, 1}, {3, 1, 3}} {
		frame, top := Frame(lines, tc.top, tc.height)
		if top != tc.want || strings.Count(frame, "\n") >= max(2, tc.height) {
			t.Errorf("%+v: top %d, %q", tc, top, frame)
		}
	}
	if Key([]byte("\x1b[A")) != 'k' || Key([]byte("\x1b[B")) != 'j' || Key([]byte("q")) != 'q' || Key(nil) != 0 {
		t.Error("keys")
	}
	if Scroll('G', 0, 10) < 1000 || Scroll('u', 9, 10) != 1 || Scroll('x', 5, 10) != 5 {
		t.Error("scrolling")
	}
}

func clients(objects ...runtime.Object) (Clients, *fake.Clientset, *kaifake.Clientset) {
	scheme := metadatafake.NewTestScheme()
	_ = metav1.AddMetaToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	node := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{Name: "n0", Labels: map[string]string{"nvidia.com/gpu.memory": "10000"}}}
	kube, kai := fake.NewSimpleClientset(), kaifake.NewSimpleClientset()
	for _, o := range objects {
		switch o := o.(type) {
		case *v1.Pod:
			_ = kube.Tracker().Add(o)
		default:
			_ = kai.Tracker().Add(o)
		}
	}
	return Clients{kube, kai, metadatafake.NewSimpleMetadataClient(scheme, node)}, kube, kai
}

func start(t *testing.T, c Clients, allPods bool, namespaces ...string) *Watcher {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	w, err := Start(ctx, c, FixedSettings(config(t)), namespaces, allPods)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Synced(ctx) {
		t.Fatal("not synced")
	}
	return w
}

// until waits for changes, as the screen does, and never polls.
func until(t *testing.T, w *Watcher, what string, ok func(*View) bool) {
	t.Helper()
	end := time.After(10 * time.Second)
	for !ok(Compute(config(t), "alice", w.Data())) {
		select {
		case <-w.Changed():
		case <-end:
			t.Fatalf("no change: %s", what)
		}
	}
}

func TestFollowsChanges(t *testing.T) {
	g, p := job("a", "alice", "high", "small", 1, 5)
	c, kube, kai := clients(g, p)
	w := start(t, c, false, "team")
	has := func(n int) func(*View) bool { return func(v *View) bool { return len(v.Jobs) == n } }
	until(t, w, "first", has(1))
	if w.Data().GPUMemory["n0"] != 10000 {
		t.Error("node labels")
	}
	if s := w.Data().Pods[0].Spec.Containers[0]; s.Env != nil || s.Command != nil {
		t.Error("kept more of the pod than is used")
	}
	ctx := context.Background()
	started := p.DeepCopy()
	started.Spec.NodeName, started.Status.Phase = "n0", v1.PodRunning
	if _, err := kube.CoreV1().Pods("team").Update(ctx, started, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	until(t, w, "started", func(v *View) bool { return v.Used == 1000 })
	if err := kai.SchedulingV2alpha2().PodGroups("team").Delete(ctx, "a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	until(t, w, "PodGroup deleted", func(v *View) bool { return len(v.Jobs) == 1 && v.Jobs[0].Gone && v.Used == 1000 })
	if err := kube.CoreV1().Pods("team").Delete(ctx, "a-0", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	until(t, w, "pod deleted", has(0))
	if len(w.Data().Former) != 0 {
		t.Error("remembers what is gone")
	}
}

func deny(f *clienttesting.Fake, resource, namespace string) {
	f.PrependReactor("list", resource, func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() != namespace {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", nil)
	})
}

func TestFallsBackToNamespaces(t *testing.T) {
	g, p := job("a", "alice", "high", "small", 1, 5)
	c, kube, kai := clients(g, p)
	deny(&kai.Fake, "podgroups", "")
	deny(&kube.Fake, "pods", "")
	deny(&kube.Fake, "pods", "private")
	w := start(t, c, true, "private", "team")
	v := Compute(config(t), "alice", w.Data())
	if len(v.Jobs) != 1 || len(v.Notes) != 3 {
		t.Errorf("jobs %v, notes %q", v.Jobs, v.Notes)
	}
	deny(&kai.Fake, "podgroups", "team")
	if _, err := Start(context.Background(), c, FixedSettings(config(t)), []string{"team"}, false); err == nil {
		t.Error("no error, with nothing to list")
	}
}

func TestOnlyReads(t *testing.T) {
	g, p := job("a", "alice", "high", "small", 1, 5)
	c, kube, kai := clients(g, p)
	w := start(t, c, true, "team")
	until(t, w, "first", func(v *View) bool { return len(v.Jobs) == 1 })
	for _, a := range append(kube.Actions(), kai.Actions()...) {
		if a.GetVerb() != "list" && a.GetVerb() != "watch" {
			t.Errorf("%s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

func TestLostConnection(t *testing.T) {
	c, _, _ := clients()
	w := start(t, c, false, "team")
	for range len(w.changed) {
		<-w.changed
	}
	w.connection("pods", fmt.Errorf("refused"))
	if d := w.Data(); d.Lost != "pods: refused" || len(w.changed) != 1 {
		t.Errorf("lost %q", d.Lost)
	}
	if text := draw(t, Compute(config(t), "alice", w.Data()), 0); !strings.Contains(text, "NOT UP TO DATE") {
		t.Error("not shown")
	}
	w.connection("pods", nil)
	if d := w.Data(); d.Lost != "" {
		t.Errorf("lost %q", d.Lost)
	}
}

// A namespace that works must not hide that another does not.
func TestLostNamespace(t *testing.T) {
	c, _, kai := clients()
	deny(&kai.Fake, "podgroups", "")
	w := start(t, c, false, "a", "b")
	w.connection("pods in a", fmt.Errorf("refused"))
	w.connection("pods in b", nil)
	if d := w.Data(); d.Lost != "pods in a: refused" {
		t.Errorf("lost %q", d.Lost)
	}
	if len(w.pods) != 2 || len(w.groups) != 2 {
		t.Error("informers")
	}
}

func TestSettingsFromConfigMap(t *testing.T) {
	text := func(budget string) string {
		return "mode: enforce\nuserLabel: example.com/user\nbudgetedClasses: high\nbudget: \"" + budget + "\"\nprotectedPriority: \"75\"\n"
	}
	c, kube, _ := clients()
	maps := kube.CoreV1().ConfigMaps("public")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := WatchSettings(ctx, kube, "public", "settings"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing: %v", err)
	}
	m := &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "public", Name: "settings"}, Data: map[string]string{SettingsKey: text("4")}}
	other := &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "public", Name: "other"}, Data: map[string]string{SettingsKey: text("9")}}
	for _, o := range []*v1.ConfigMap{m, other} {
		if _, err := maps.Create(ctx, o, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := WatchSettings(ctx, kube, "public", "settings")
	if err != nil {
		t.Fatal(err)
	}
	w, err := Start(ctx, c, s, []string{"team"}, false)
	if err != nil || !w.Synced(ctx) {
		t.Fatal(err)
	}
	wait := func(what string, budget tiers.Units, problem string) {
		t.Helper()
		for {
			config, p := s.Config()
			if config.Policy.Budget == budget && strings.Contains(p, problem) && (problem != "" || p == "") {
				return
			}
			select {
			case <-w.Changed():
			case <-ctx.Done():
				t.Fatalf("%s: budget %v, problem %q", what, config.Policy.Budget, p)
			}
		}
	}
	wait("first", 4000, "")
	set := func(text string) {
		t.Helper()
		m = m.DeepCopy()
		m.Data = map[string]string{SettingsKey: text}
		if _, err := maps.Update(ctx, m, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	set(text("6"))
	wait("changed", 6000, "")
	set("budget: 4")
	wait("invalid", 6000, "userLabel: required")
	if notes := w.Data().Notes; len(notes) != 1 || !strings.Contains(notes[0], "out of date") {
		t.Errorf("notes %q", notes)
	}
	set(strings.Replace(text("7"), "userLabel", "ownerLabel", 1))
	wait("old name", 6000, "unknown argument")
	set(text("8"))
	wait("valid again", 8000, "")
	if err := maps.Delete(ctx, "settings", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	wait("deleted", 8000, "does not exist")
}

func TestGPUTypes(t *testing.T) {
	d := data()
	d.NodeTypes = map[string]string{"n0": "big"}
	d.with(job("chosen", "alice", "high", "small", 1, 9, runs(8)))
	d.with(job("placed", "alice", "mid", "", 2, 8, runs(7)))
	d.with(job("waits", "alice", "high", "", 1, 5))
	v := Compute(config(t), "alice", d)
	for name, want := range map[string]struct {
		gpuType     string
		gpus, units tiers.Units
	}{"chosen": {"small", 1000, 1000}, "placed": {"big", 2000, 4000}, "waits": {anyType, 1000, 2000}} {
		if j := find(t, v, name); !slices.Equal(j.Types, []string{want.gpuType}) || j.GPUs != want.gpus || j.Units != want.units {
			t.Errorf("%s: %v, %v GPUs, %v units", name, j.Types, j.GPUs, j.Units)
		}
	}
	text := draw(t, v, 0)
	for _, want := range []string{"Uses   2 big, 1 small\n", "Waits  1 any type\n", "GPUs of any type: you are number 1 of 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	// A charge follows what was asked for, whatever the node.
	d.NodeTypes = nil
	if j := find(t, Compute(config(t), "alice", d), "placed"); j.Units != 4000 || j.Types[0] != anyType {
		t.Errorf("%+v", j)
	}
}

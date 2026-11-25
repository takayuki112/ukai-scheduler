// Command load submits random jobs to a test cluster with simulated nodes, checks at
// every pod event that no owner holds more than the budget, and reports the scheduler's
// latency and memory. See test/scale/run.sh.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

const (
	namespace  = "default"
	testLabel  = "ukai-load"
	ownerLabel = "example.com/owner"
	gpu        = "nvidia.com/gpu"
)

var (
	kubeconfig = flag.String("kubeconfig", "", "kubeconfig of the test cluster")
	contextArg = flag.String("context", "", "refuse any other current context")
	owners     = flag.Int("owners", 20, "number of owners")
	budget     = flag.Int64("budget", 8, "budget to check, or 0 not to")
	burst      = flag.Int("jobs", 0, "jobs submitted at the start")
	rate       = flag.Float64("rate", 0, "further jobs per minute")
	duration   = flag.Duration("duration", 10*time.Minute, "how long to run")
	runMin     = flag.Duration("run-min", 20*time.Second, "shortest job")
	runMax     = flag.Duration("run-max", 2*time.Minute, "longest job")
	deleted    = flag.Float64("deleted", 0.1, "share of arrivals at which a random job is deleted")
	drain      = flag.Duration("drain", 0, "at the end, wait this long for all jobs to finish, and fail if they do not")
	gangs      = flag.Bool("gangs", true, "some jobs are gangs of two pods")
	seed       = flag.Int64("seed", 1, "random seed")
	out        = flag.String("out", "", "CSV file for samples")
)

type podState struct {
	owner, class, node string
	units              int64
	live               bool // bound and not finished
	created, started   time.Time
}

// watcher follows every pod event.
type watcher struct {
	sync.Mutex
	pods       map[string]*podState
	held       map[string]int64 // by owner, P1 only
	events     int
	violations int
	started    map[string]int
	evicted    map[string]int
	finished   int
	waits      map[string][]time.Duration
	short      int // evicted within 30 s of starting
}

func live(pod *v1.Pod) bool {
	return pod.Spec.NodeName != "" && pod.Status.Phase != v1.PodSucceeded && pod.Status.Phase != v1.PodFailed
}

func (w *watcher) update(pod *v1.Pod, gone bool) {
	w.Lock()
	defer w.Unlock()
	w.events++
	key := string(pod.UID)
	old := w.pods[key]
	if old == nil {
		old = &podState{
			owner: pod.Labels[ownerLabel], class: pod.Spec.PriorityClassName, created: pod.CreationTimestamp.Time,
			units: pod.Spec.Containers[0].Resources.Limits.Name(gpu, resource.DecimalSI).Value(),
		}
		w.pods[key] = old
	}
	now := !gone && live(pod)
	if now && !old.live {
		old.started = time.Now()
		w.started[old.class]++
		w.waits[old.class] = append(w.waits[old.class], old.started.Sub(old.created))
	}
	if !old.live && !now && gone {
		delete(w.pods, key)
		return
	}
	if old.live && !now && pod.Status.Phase == v1.PodSucceeded {
		w.finished++
	}
	if old.class == "test-high" && now != old.live {
		if now {
			w.held[old.owner] += old.units
			if *budget > 0 && w.held[old.owner] > *budget {
				w.violations++
				log.Printf("OVER BUDGET: %s holds %d after %s", old.owner, w.held[old.owner], pod.Name)
			}
		} else {
			w.held[old.owner] -= old.units
		}
	}
	// KAI sets this condition on pods it evicts, as run.sh asks it to.
	if old.live && !now && pod.Status.Phase != v1.PodSucceeded {
		for _, c := range pod.Status.Conditions {
			if c.Type == v1.DisruptionTarget && c.Status == v1.ConditionTrue {
				w.evicted[old.class]++
				if time.Since(old.started) < 30*time.Second {
					w.short++
				}
			}
		}
	}
	old.live = now
	if gone {
		delete(w.pods, key)
	}
}

func (w *watcher) counts() (pending, running int) {
	w.Lock()
	defer w.Unlock()
	for _, p := range w.pods {
		if p.live {
			running++
		}
	}
	return len(w.pods) - running, running
}

type driver struct {
	client kubernetes.Interface
	r      *rand.Rand
	run    int64 // makes names unique
	next   int
	names  []string
}

func (d *driver) submit(ctx context.Context) error {
	class := []string{"test-top", "test-high", "test-high", "test-high", "test-mid", "test-mid", "test-mid", "test-mid", "test-low", "test-low"}[d.r.Intn(10)]
	owner := fmt.Sprintf("owner-%d", d.r.Intn(*owners))
	units := []int64{1, 1, 1, 2, 2, 4, 8}[d.r.Intn(7)]
	pods := []int32{1, 1, 1, 1, 2}[d.r.Intn(5)]
	if units > 4 || !*gangs {
		pods = 1
	}
	name := fmt.Sprintf("load-%d-%d", d.run, d.next)
	run := *runMin + time.Duration(d.r.Int63n(int64(*runMax-*runMin)+1))
	d.next++
	labels := map[string]string{testLabel: "1", ownerLabel: owner, "kai.scheduler/queue": "default-queue"}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: map[string]string{"kai.scheduler/batch-min-member": strconv.Itoa(int(pods))}},
		Spec: batchv1.JobSpec{
			Parallelism: &pods, Completions: &pods, BackoffLimit: ptr.To(int32(0)), TTLSecondsAfterFinished: ptr.To(int32(5)),
			PodFailurePolicy: &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{{
				Action:          batchv1.PodFailurePolicyActionIgnore,
				OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{{Type: v1.DisruptionTarget, Status: v1.ConditionTrue}},
			}}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{"example.com/run-for": run.Round(time.Second).String()}},
				Spec: v1.PodSpec{
					SchedulerName: "kai-scheduler", PriorityClassName: class, RestartPolicy: v1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					NodeSelector:                 map[string]string{"type": "kwok"},
					Tolerations:                  []v1.Toleration{{Key: "kwok.x-k8s.io/node", Operator: v1.TolerationOpExists}},
					Containers: []v1.Container{{
						Name: "main", Image: "none",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
							Limits:   v1.ResourceList{gpu: *resource.NewQuantity(units, resource.DecimalSI)},
						},
					}},
				},
			},
		},
	}
	if _, err := d.client.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return err
	}
	d.names = append(d.names, name)
	return nil
}

func (d *driver) deleteOne(ctx context.Context) {
	if len(d.names) > 0 {
		i := d.r.Intn(len(d.names))
		_ = d.client.BatchV1().Jobs(namespace).Delete(ctx, d.names[i], metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationBackground)})
		d.names = slices.Delete(d.names, i, i+1)
	}
}

// scrape returns the scheduler's metrics, by their name with labels, and its restarts.
func scrape(ctx context.Context, client kubernetes.Interface) (map[string]float64, int32, error) {
	pods, err := client.CoreV1().Pods("kai-scheduler").List(ctx, metav1.ListOptions{LabelSelector: "app=kai-scheduler-default"})
	if err != nil {
		return nil, 0, err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != v1.PodRunning || pod.DeletionTimestamp != nil {
			continue
		}
		body, err := client.CoreV1().Pods("kai-scheduler").ProxyGet("http", pod.Name, "8080", "metrics", nil).DoRaw(ctx)
		if err != nil {
			return nil, pod.Status.ContainerStatuses[0].RestartCount, err
		}
		m := map[string]float64{}
		for s := bufio.NewScanner(bytes.NewReader(body)); s.Scan(); {
			if i := strings.LastIndexByte(s.Text(), ' '); i > 0 && s.Text()[0] != '#' {
				m[s.Text()[:i]], _ = strconv.ParseFloat(s.Text()[i+1:], 64)
			}
		}
		return m, pod.Status.ContainerStatuses[0].RestartCount, nil
	}
	return nil, 0, fmt.Errorf("no scheduler")
}

var columns = map[string]string{
	"cycle":   "kai_e2e_scheduling_latency_milliseconds",
	"open":    "kai_open_session_latency_milliseconds",
	"plugin":  `kai_plugin_scheduling_latency_milliseconds{OnSession="OnSessionOpen",plugin="tierbudget"}`,
	"consol":  `kai_action_scheduling_latency_milliseconds{action="ukai-consolidation"}`,
	"stockco": `kai_action_scheduling_latency_milliseconds{action="consolidation"}`,
	"alloc":   `kai_action_scheduling_latency_milliseconds{action="allocate"}`,
	"preempt": `kai_action_scheduling_latency_milliseconds{action="preempt"}`,
	"rss":     "process_resident_memory_bytes",
	"heap":    "go_memstats_heap_inuse_bytes",
	"gor":     "go_goroutines",
	"panics":  "ukai_panics_total",
}

func percentile[T int | float64 | time.Duration](v []T, p float64) T {
	if len(v) == 0 {
		return 0
	}
	v = slices.Clone(v)
	slices.Sort(v)
	return v[int(p*float64(len(v)-1))]
}

func main() {
	flag.Parse()
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfig}, nil)
	raw, err := loader.RawConfig()
	check(err)
	if *kubeconfig == "" || *contextArg == "" || raw.CurrentContext != *contextArg {
		log.Fatalf("the current context is %q, not %q", raw.CurrentContext, *contextArg)
	}
	config, err := loader.ClientConfig()
	check(err)
	config.QPS, config.Burst = 100, 200
	client, err := kubernetes.NewForConfig(config)
	check(err)
	ctx := context.Background()

	w := &watcher{pods: map[string]*podState{}, held: map[string]int64{}, started: map[string]int{}, evicted: map[string]int{}, waits: map[string][]time.Duration{}}
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = testLabel }))
	_, err = factory.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { w.update(o.(*v1.Pod), false) },
		UpdateFunc: func(_, o any) { w.update(o.(*v1.Pod), false) },
		DeleteFunc: func(o any) {
			if t, ok := o.(cache.DeletedFinalStateUnknown); ok {
				o = t.Obj
			}
			w.update(o.(*v1.Pod), true)
		},
	})
	check(err)
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	if len(w.pods) > 0 {
		log.Fatalf("%d pods of an earlier run are left", len(w.pods))
	}
	csv := os.Stdout
	if *out != "" {
		csv, err = os.Create(*out)
		check(err)
	}
	names := []string{}
	for c := range columns {
		names = append(names, c)
	}
	slices.Sort(names)
	fmt.Fprintln(csv, "seconds,pending,running,violations,"+strings.Join(names, ","))

	d := &driver{client: client, r: rand.New(rand.NewSource(*seed)), run: time.Now().Unix()}
	start := time.Now()
	samples := map[string][]float64{}
	failed, restarts := 0, int32(0)
	sample := func() {
		pending, running := w.counts()
		m, n, err := scrape(ctx, client)
		restarts = max(restarts, n)
		if err != nil {
			failed++
			log.Printf("metrics: %v", err)
			return
		}
		row := []string{}
		for _, c := range names {
			samples[c] = append(samples[c], m[columns[c]])
			row = append(row, strconv.FormatFloat(m[columns[c]], 'f', -1, 64))
		}
		fmt.Fprintf(csv, "%.0f,%d,%d,%d,%s\n", time.Since(start).Seconds(), pending, running, w.violations, strings.Join(row, ","))
	}
	errs := 0
	for range *burst {
		if err := d.submit(ctx); err != nil {
			errs++
		}
	}
	log.Printf("submitted %d jobs in %s", *burst, time.Since(start).Round(time.Second))
	due, lastSample, lastLog := 0.0, time.Now(), time.Now()
	last := time.Now()
	for time.Since(start) < *duration {
		due += time.Since(last).Minutes() * *rate
		last = time.Now()
		for ; due >= 1; due-- {
			if err := d.submit(ctx); err != nil {
				errs++
			}
			if d.r.Float64() < *deleted {
				d.deleteOne(ctx)
			}
		}
		if time.Since(lastSample) >= 2*time.Second {
			lastSample = time.Now()
			sample()
		}
		if time.Since(lastLog) >= time.Minute {
			lastLog = time.Now()
			pending, running := w.counts()
			log.Printf("%s: %d pending, %d running, %d finished, %d events, %d violations", time.Since(start).Round(time.Second), pending, running, w.finished, w.events, w.violations)
		}
		time.Sleep(200 * time.Millisecond)
	}
	stuck := 0
	if *drain > 0 {
		for end := time.Now().Add(*drain); time.Now().Before(end); time.Sleep(2 * time.Second) {
			sample()
			jobs, err := client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: testLabel})
			if err == nil {
				if stuck = len(jobs.Items); stuck == 0 {
					break
				}
			}
		}
		log.Printf("drained, %d jobs left", stuck)
	}
	check(client.BatchV1().Jobs(namespace).DeleteCollection(ctx, metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationBackground)}, metav1.ListOptions{LabelSelector: testLabel}))
	for end := time.Now().Add(15 * time.Minute); time.Now().Before(end); time.Sleep(time.Second) {
		if p, r := w.counts(); p+r == 0 {
			break
		}
	}
	time.Sleep(20 * time.Second)
	sample()

	w.Lock()
	defer w.Unlock()
	fmt.Printf("jobs submitted %d (%d errors), pods finished %d, events %d, violations %d, restarts %d, failed scrapes %d of %d\n",
		d.next, errs, w.finished, w.events, w.violations, restarts, failed, failed+len(samples["cycle"]))
	for _, c := range []string{"test-top", "test-high", "test-mid", "test-low"} {
		fmt.Printf("%-10s started %5d  evicted %5d  wait p50 %s p95 %s\n", c, w.started[c], w.evicted[c],
			percentile(w.waits[c], 0.5).Round(time.Second), percentile(w.waits[c], 0.95).Round(time.Second))
	}
	fmt.Printf("evicted within 30 s of starting: %d\n", w.short)
	for _, c := range names {
		v := samples[c]
		scale, unit := 1.0, ""
		if c == "rss" || c == "heap" {
			scale, unit = 1<<20, " MiB"
		}
		fmt.Printf("%-8s p50 %8.1f  p95 %8.1f  max %8.1f  last %8.1f%s\n", c, percentile(v, 0.5)/scale, percentile(v, 0.95)/scale, slices.Max(v)/scale, v[len(v)-1]/scale, unit)
	}
	if w.violations > 0 || restarts > 0 || stuck > 0 || errs > 0 || slices.Max(samples["panics"]) > 0 || len(w.pods) > 0 {
		log.Fatal("FAIL")
	}
	fmt.Println("PASS")
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

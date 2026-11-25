package dashboard

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"

	kaiclient "github.com/kai-scheduler/KAI-scheduler/pkg/apis/client/clientset/versioned"
	enginev2alpha2 "github.com/kai-scheduler/KAI-scheduler/pkg/apis/scheduling/v2alpha2"
	"github.com/kai-scheduler/KAI-scheduler/pkg/common/constants"
)

// Clients are the API, as the user.
type Clients struct {
	Kube     kubernetes.Interface
	KAI      kaiclient.Interface
	Metadata metadata.Interface
}

// Watcher follows the API with watches.
type Watcher struct {
	settings   *Settings
	namespaces []string
	allPods    bool
	changed    chan struct{}
	groups     []cache.SharedInformer
	pods       []cache.SharedInformer
	nodes      []cache.SharedInformer
	notes      []string
	seesPods   map[string]bool

	mu     sync.Mutex
	lost   map[string]string
	former map[string]Group
}

// Start asks what may be listed, and watches that: PodGroups of all namespaces if
// allowed, or else of the given ones; pods of the given ones, or with allPods of all if
// allowed; and the labels of nodes if allowed.
func Start(ctx context.Context, clients Clients, settings *Settings, namespaces []string, allPods bool) (*Watcher, error) {
	w := &Watcher{
		settings: settings, namespaces: namespaces, changed: make(chan struct{}, 1),
		seesPods: map[string]bool{}, lost: map[string]string{}, former: map[string]Group{},
	}
	settings.onChange(w.notify)
	groups := func(ns string) lister {
		c := clients.KAI.SchedulingV2alpha2().PodGroups(ns)
		return lister{
			func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return c.List(ctx, o) }, c.Watch}
	}
	pods := func(ns string) lister {
		c := clients.Kube.CoreV1().Pods(ns)
		return lister{
			func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return c.List(ctx, o) }, c.Watch}
	}
	nodes := clients.Metadata.Resource(v1.SchemeGroupVersion.WithResource("nodes"))

	all, err := allowed(ctx, groups(""))
	if err != nil {
		return nil, fmt.Errorf("listing PodGroups: %w", err)
	}
	scope := []string{""}
	if !all {
		w.notes = append(w.notes, "You may not list PodGroups of all namespaces: the queue, and your budget, may be incomplete.")
		scope = namespaces
	}
	for _, ns := range scope {
		if ok, err := allowed(ctx, groups(ns)); err != nil {
			return nil, fmt.Errorf("listing PodGroups: %w", err)
		} else if !ok {
			w.notes = append(w.notes, "You may not list PodGroups in "+ns+".")
			continue
		}
		w.groups = append(w.groups, w.informer(place("PodGroups", ns), groups(ns), &enginev2alpha2.PodGroup{}, nil))
	}

	scope = namespaces
	if allPods {
		ok, err := allowed(ctx, pods(""))
		if err != nil {
			return nil, fmt.Errorf("listing pods: %w", err)
		}
		if ok {
			w.allPods, scope = true, []string{""}
		} else {
			w.notes = append(w.notes, "You may not list pods of all namespaces.")
		}
	}
	for _, ns := range scope {
		if ok, err := allowed(ctx, pods(ns)); err != nil {
			return nil, fmt.Errorf("listing pods: %w", err)
		} else if !ok {
			w.notes = append(w.notes, "You may not list pods in "+ns+".")
			continue
		}
		w.seesPods[ns] = true
		w.pods = append(w.pods, w.informer(place("pods", ns), pods(ns), &v1.Pod{}, stripPod))
	}
	nodeLister := lister{
		func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return nodes.List(ctx, o) }, nodes.Watch}
	if ok, err := allowed(ctx, nodeLister); err == nil && ok {
		w.nodes = append(w.nodes, w.informer("nodes", nodeLister, &metav1.PartialObjectMetadata{}, nil))
	}

	if len(w.groups) == 0 {
		return nil, fmt.Errorf("you may not list PodGroups in any of the namespaces %v", namespaces)
	}
	for _, i := range w.informers() {
		go i.Run(ctx.Done())
	}
	return w, nil
}

// Changed receives when there is something new to show.
func (w *Watcher) Changed() <-chan struct{} { return w.changed }

// Synced waits for the first lists.
func (w *Watcher) Synced(ctx context.Context) bool {
	var synced []cache.InformerSynced
	for _, i := range w.informers() {
		synced = append(synced, i.HasSynced)
	}
	return cache.WaitForCacheSync(ctx.Done(), synced...)
}

func (w *Watcher) informers() []cache.SharedInformer { return slices.Concat(w.groups, w.pods, w.nodes) }

// Data is the present state.
func (w *Watcher) Data() Data {
	config, problem := w.settings.Config()
	d := Data{GPUMemory: map[string]int64{}, NodeTypes: map[string]string{}, Notes: w.notes, SeesPods: func(ns string) bool { return w.allPods || w.seesPods[ns] }}
	for _, i := range w.groups {
		for _, o := range i.GetStore().List() {
			d.Groups = append(d.Groups, o.(*enginev2alpha2.PodGroup))
		}
	}
	for _, i := range w.pods {
		for _, o := range i.GetStore().List() {
			d.Pods = append(d.Pods, o.(*v1.Pod))
		}
	}
	for _, i := range w.nodes {
		for _, o := range i.GetStore().List() {
			n := o.(*metav1.PartialObjectMetadata)
			if config.GPUTypeLabel != "" {
				d.NodeTypes[n.Name] = n.Labels[config.GPUTypeLabel]
			}
			d.GPUMemory[n.Name] = parseGPUMemory(n.Labels[constants.NvidiaGpuMemory])
		}
	}
	if problem != "" {
		d.Notes = append(slices.Clone(d.Notes), "The settings may be out of date: "+problem)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// Remember PodGroups for as long as they have pods.
	former := map[string]Group{}
	for _, g := range d.Groups {
		former[g.Namespace+"/"+g.Name] = Group{UserOf(config, g.Labels), g.Spec.PriorityClassName}
	}
	for _, p := range d.Pods {
		id := p.Namespace + "/" + p.Annotations[constants.PodGroupAnnotationForPod]
		if g, ok := w.former[id]; ok && former[id] == (Group{}) {
			former[id] = g
		}
	}
	w.former = former
	d.Former = maps.Clone(former)
	for _, k := range slices.Sorted(maps.Keys(w.lost)) {
		d.Lost = k + ": " + w.lost[k]
	}
	return d
}

func (w *Watcher) notify() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

func (w *Watcher) connection(what string, err error) {
	w.mu.Lock()
	before := w.lost[what]
	if err == nil {
		delete(w.lost, what)
	} else {
		w.lost[what] = err.Error()
	}
	same := before == w.lost[what]
	w.mu.Unlock()
	if !same {
		w.notify()
	}
}

func place(what, namespace string) string {
	if namespace == "" {
		return what
	}
	return what + " in " + namespace
}

type lister struct {
	list  func(context.Context, metav1.ListOptions) (runtime.Object, error)
	watch func(context.Context, metav1.ListOptions) (watch.Interface, error)
}

// allowed says whether listing is. Other errors are returned.
func allowed(ctx context.Context, l lister) (bool, error) {
	_, err := l.list(ctx, metav1.ListOptions{Limit: 1})
	if apierrors.IsForbidden(err) {
		return false, nil
	}
	return err == nil, err
}

// listWatch lists and then watches, which any server supports.
type listWatch struct{ *cache.ListWatch }

func (listWatch) IsWatchListSemanticsUnSupported() bool { return true }

func (w *Watcher) informer(what string, l lister, example runtime.Object, transform cache.TransformFunc) cache.SharedInformer {
	lw := listWatch{&cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			list, err := l.list(ctx, o)
			w.connection(what, err)
			return list, err
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			i, err := l.watch(ctx, o)
			if !apierrors.IsResourceExpired(err) && !apierrors.IsGone(err) { // it lists again then
				w.connection(what, err)
			}
			return i, err
		},
	}}
	i := cache.NewSharedInformer(lw, example, 0)
	if transform != nil {
		_ = i.SetTransform(transform) // fails only once it runs
	}
	_, _ = i.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(any) { w.notify() }, UpdateFunc: func(any, any) { w.notify() }, DeleteFunc: func(any) { w.notify() },
	})
	return i
}

// stripPod keeps only what is used, and so no environment variables or commands.
func stripPod(o any) (any, error) {
	p, ok := o.(*v1.Pod)
	if !ok {
		return o, nil
	}
	s := &v1.Pod{Spec: v1.PodSpec{
		NodeName: p.Spec.NodeName, NodeSelector: p.Spec.NodeSelector, PriorityClassName: p.Spec.PriorityClassName,
		SchedulingGates: p.Spec.SchedulingGates, ResourceClaims: p.Spec.ResourceClaims, Overhead: p.Spec.Overhead, Resources: p.Spec.Resources,
	}, Status: v1.PodStatus{
		Phase: p.Status.Phase, Conditions: p.Status.Conditions,
	}}
	s.Name, s.Namespace, s.UID, s.ResourceVersion = p.Name, p.Namespace, p.UID, p.ResourceVersion
	s.Labels, s.CreationTimestamp, s.DeletionTimestamp = p.Labels, p.CreationTimestamp, p.DeletionTimestamp
	s.Annotations = maps.Clone(p.Annotations)
	delete(s.Annotations, v1.LastAppliedConfigAnnotation)
	for _, c := range p.Spec.Containers {
		s.Spec.Containers = append(s.Spec.Containers, v1.Container{Name: c.Name, Resources: c.Resources, RestartPolicy: c.RestartPolicy})
	}
	for _, c := range p.Spec.InitContainers {
		s.Spec.InitContainers = append(s.Spec.InitContainers, v1.Container{Name: c.Name, Resources: c.Resources, RestartPolicy: c.RestartPolicy})
	}
	for _, c := range p.Status.ContainerStatuses { // for pods resized in place
		s.Status.ContainerStatuses = append(s.Status.ContainerStatuses, v1.ContainerStatus{Name: c.Name, Resources: c.Resources, AllocatedResources: c.AllocatedResources})
	}
	return s, nil
}

package dashboard

import (
	"context"
	"fmt"
	"sync"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"

	"github.com/takayuki112/ukai-scheduler/pkg/tierbudget"
)

// SettingsKey is the key in the ConfigMap with the settings.
const SettingsKey = "arguments.yaml"

// Settings are the plugin's arguments, which admins may change.
type Settings struct {
	mu      sync.Mutex
	config  *tierbudget.Config
	problem string
	changed func()
}

// ParseSettings reads a YAML map of the plugin's arguments.
func ParseSettings(text []byte) (*tierbudget.Config, error) {
	var args map[string]string
	if err := yaml.UnmarshalStrict(text, &args); err != nil {
		return nil, fmt.Errorf("all values must be strings: %w", err)
	}
	return tierbudget.ParseConfig(args)
}

// FixedSettings never change.
func FixedSettings(config *tierbudget.Config) *Settings { return &Settings{config: config} }

// WatchSettings follows a ConfigMap. It fails if that cannot be used now. If it cannot
// later, the last settings that could stay, and Problem says why.
func WatchSettings(ctx context.Context, client kubernetes.Interface, namespace, name string) (*Settings, error) {
	maps := client.CoreV1().ConfigMaps(namespace)
	only := fields.OneTermEqualSelector(metav1.ObjectNameField, name).String()
	where := "ConfigMap " + namespace + "/" + name
	list, err := maps.List(ctx, metav1.ListOptions{FieldSelector: only})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	s := &Settings{}
	s.set(where, nil)
	for i := range list.Items {
		if list.Items[i].Name == name {
			s.set(where, &list.Items[i])
		}
	}
	if s.config == nil {
		return nil, fmt.Errorf("%s", s.problem)
	}
	informer := cache.NewSharedInformer(listWatch{&cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			o.FieldSelector = only
			return maps.List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.FieldSelector = only
			return maps.Watch(ctx, o)
		},
	}}, &v1.ConfigMap{}, 0)
	update := func(o any) {
		if m, ok := o.(*v1.ConfigMap); ok && m.Name == name {
			s.set(where, m)
		}
	}
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: update, UpdateFunc: func(_, o any) { update(o) },
		DeleteFunc: func(o any) {
			if t, ok := o.(cache.DeletedFinalStateUnknown); ok {
				o = t.Obj
			}
			if m, ok := o.(*v1.ConfigMap); ok && m.Name == name {
				s.set(where, nil)
			}
		},
	})
	go informer.Run(ctx.Done())
	return s, nil
}

func (s *Settings) set(where string, m *v1.ConfigMap) {
	problem := where + " does not exist"
	var config *tierbudget.Config
	if m != nil {
		problem = where + " has no " + SettingsKey
		if text, ok := m.Data[SettingsKey]; ok {
			var err error
			if config, err = ParseSettings([]byte(text)); err != nil {
				config, problem = nil, where+": "+err.Error()
			}
		}
	}
	s.mu.Lock()
	if config != nil {
		s.config, problem = config, ""
	}
	s.problem = problem
	changed := s.changed
	s.mu.Unlock()
	if changed != nil {
		changed()
	}
}

// Config are the settings to use, and what is wrong with newer ones.
func (s *Settings) Config() (config *tierbudget.Config, problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config, s.problem
}

func (s *Settings) onChange(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changed = f
}

// Package tierbudget is a KAI scheduler plugin. It currently registers no hooks
// and does not affect scheduling.
package tierbudget

import (
	"sync"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/log"
)

// Name is the plugin's name in the scheduler configuration.
const Name = "tierbudget"

// KAI builds plugins anew each session; log the first load only.
var logLoaded sync.Once

type plugin struct{}

// New builds the plugin. It is a framework.PluginBuilder.
func New(framework.PluginArguments) framework.Plugin {
	return &plugin{}
}

func (*plugin) Name() string { return Name }

func (*plugin) OnSessionOpen(*framework.Session) {
	logLoaded.Do(func() {
		log.InfraLogger.Infof("Plugin %s loaded", Name)
	})
}

func (*plugin) OnSessionClose(*framework.Session) {}

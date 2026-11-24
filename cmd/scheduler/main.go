// Command scheduler is the stock KAI scheduler with an additional plugin and action.
package main

import (
	"fmt"
	"os"

	"github.com/kai-scheduler/KAI-scheduler/cmd/scheduler/app"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"

	"github.com/takayuki112/ukai-scheduler/pkg/tierbudget"
)

func main() {
	// KAI adds its own by name in RunApp, which leaves these registered.
	framework.RegisterPluginBuilder(tierbudget.Name, tierbudget.New)
	framework.RegisterAction(tierbudget.NewConsolidation())

	if err := app.RunApp(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

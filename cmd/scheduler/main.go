// Command scheduler is the stock KAI scheduler with additional plugins.
package main

import (
	"fmt"
	"os"

	"github.com/kai-scheduler/KAI-scheduler/cmd/scheduler/app"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"

	"github.com/takayuki112/ukai-scheduler/pkg/tierbudget"
)

func main() {
	// KAI adds its own plugins by name in RunApp, which leaves this one registered.
	framework.RegisterPluginBuilder(tierbudget.Name, tierbudget.New)

	if err := app.RunApp(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

package tierbudget

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	enginev2alpha2 "github.com/kai-scheduler/KAI-scheduler/pkg/apis/scheduling/v2alpha2"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/actions/consolidation"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/podgroup_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/log"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// ConsolidationName is the action's name in the scheduler configuration. KAI's own
// "consolidation" must be disabled there, and cannot be replaced by name because KAI
// registers it later.
const ConsolidationName = "ukai-consolidation"

// promotedGroup stands for promoted jobs in a pass, in place of their class.
const promotedGroup = "@promoted"

// Pass is one run of KAI's consolidation, limited to some jobs. Each entry is a
// priority class, or promotedGroup.
type Pass struct {
	Place []string // pending jobs that room may be made for
	Move  []string // running jobs that may be restarted elsewhere
}

// openSession lets the action, which KAI creates once and without arguments, find the
// plugin of the session it runs in. It is set only if that plugin enforces.
var openSession atomic.Pointer[plugin]

// consolidationAction runs KAI's consolidation, which would otherwise restart jobs of
// any priority to place a job of any other. It hides the jobs that a pass excludes by
// marking them non-preemptible while it runs. They take no part in the pass, so KAI
// does not account for them differently.
type consolidationAction struct {
	stock framework.Action
}

// NewConsolidation builds the action. Without an enforcing plugin in the session it
// is KAI's consolidation unchanged.
func NewConsolidation() framework.Action {
	return &consolidationAction{stock: consolidation.New()}
}

func (*consolidationAction) Name() framework.ActionType { return ConsolidationName }

func (a *consolidationAction) Execute(ssn *framework.Session) {
	p := openSession.Load()
	if p == nil || p.ssn != ssn {
		a.stock.Execute(ssn)
		return
	}
	if len(p.config.Passes) == 0 {
		a.runPass(ssn, p.allowedByDefault)
		return
	}
	for _, pass := range p.config.Passes {
		a.runPass(ssn, func(info *podgroup_info.PodGroupInfo) bool { return p.allowedIn(pass, info) })
	}
}

func (a *consolidationAction) runPass(ssn *framework.Session, allowed func(*podgroup_info.PodGroupInfo) bool) {
	original := map[*podgroup_info.PodGroupInfo]enginev2alpha2.Preemptibility{}
	defer func() {
		for info, value := range original {
			info.Preemptibility = value
		}
	}()
	for _, info := range ssn.ClusterInfo.PodGroupInfos {
		if info != nil && info.HasEvictableTasks() && !allowed(info) {
			original[info] = info.Preemptibility
			info.Preemptibility = enginev2alpha2.NonPreemptible
		}
	}
	a.stock.Execute(ssn)
}

// allowedByDefault protects running jobs that are budgeted, charged or promoted.
func (p *plugin) allowedByDefault(info *podgroup_info.PodGroupInfo) bool {
	state := p.jobs[info.UID]
	if state == nil || info.GetActiveAllocatedTasksCount() == 0 {
		return true
	}
	return state.class != tiers.Budgeted && state.class != tiers.Charged && !p.promoted(info)
}

// allowedIn says whether a job takes part in a pass. A job that is partly running must
// be one that may be moved, even if it is only to be completed.
func (p *plugin) allowedIn(pass Pass, info *podgroup_info.PodGroupInfo) bool {
	group := promotedGroup
	if !p.promoted(info) {
		if info.PodGroup == nil {
			return false
		}
		group = info.PodGroup.Spec.PriorityClassName
	}
	if info.GetActiveAllocatedTasksCount() > 0 {
		return slices.Contains(pass.Move, group)
	}
	return slices.Contains(pass.Place, group)
}

func (p *plugin) promoted(info *podgroup_info.PodGroupInfo) bool {
	return p.results[string(info.UID)].Tier == tiers.Promoted
}

// warnStockConsolidation reports a configuration that also runs KAI's own action.
func warnStockConsolidation(ssn *framework.Session) {
	if ssn.Config == nil {
		return
	}
	message := ""
	for _, name := range strings.Split(ssn.Config.Actions, ",") {
		if strings.TrimSpace(name) == string(framework.Consolidation) {
			message = fmt.Sprintf("action %s is enabled and may restart jobs of any tier; use %s instead",
				framework.Consolidation, ConsolidationName)
		}
	}
	if errorLog.changed("consolidation", message) && message != "" {
		log.InfraLogger.Warningf("Plugin %s: %s", Name, message)
	}
}

// passes parses "place,place>move,move;place>move".
func (p *parser) passes() []Pass {
	value := p.get(argConsolidation)
	if value == "" {
		return nil
	}
	var passes []Pass
	for _, item := range strings.Split(value, ";") {
		place, move, ok := strings.Cut(item, ">")
		if !ok {
			p.failf("%s: %q is not place>move", argConsolidation, strings.TrimSpace(item))
			continue
		}
		passes = append(passes, Pass{Place: p.groups(place), Move: p.groups(move)})
	}
	return passes
}

func (p *parser) groups(value string) []string {
	var groups []string
	for _, g := range strings.Split(value, ",") {
		g = strings.TrimSpace(g)
		switch {
		case g == "":
			p.failf("%s: empty item", argConsolidation)
		case strings.HasPrefix(g, "@") && g != promotedGroup:
			p.failf("%s: unknown group %q", argConsolidation, g)
		case slices.Contains(groups, g):
			p.failf("%s: %q is listed more than once", argConsolidation, g)
		}
		groups = append(groups, g)
	}
	return groups
}

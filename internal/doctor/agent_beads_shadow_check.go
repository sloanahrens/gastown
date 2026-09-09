package doctor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// AgentBeadsShadowCheck reports rig-prefixed agent beads that ALSO exist in
// the town database. Such a row is a pre-gt-8we legacy copy: bd resolves the
// local store before routes.jsonl, so a bd call from the town root reads or
// writes the stale copy while gt reads the rig row (hq-kt9y1: 10/30 gastown
// polecats diverged, two slots leaked to ghost active_mr). Detection only —
// repair is `gt polecat identity reconcile`, one ID per command.
type AgentBeadsShadowCheck struct {
	BaseCheck
}

// NewAgentBeadsShadowCheck creates a new agent-beads-shadow check.
func NewAgentBeadsShadowCheck() *AgentBeadsShadowCheck {
	return &AgentBeadsShadowCheck{
		BaseCheck: BaseCheck{
			CheckName:        "agent-beads-shadow",
			CheckDescription: "Rig-prefixed agent beads must not have a legacy copy in the town database",
			CheckCategory:    CategoryRig,
		},
	}
}

// Run scans every routed rig for agent beads that are shadowed by a legacy
// row in the town database.
func (c *AgentBeadsShadowCheck) Run(ctx *CheckContext) *CheckResult {
	res := &CheckResult{Name: c.Name(), Category: c.Category(), Status: StatusOK, Message: "No rig agent beads shadowed in the town database"}

	townBeadsDir := beads.GetTownBeadsPath(ctx.TownRoot)
	routes, err := beads.LoadRoutes(townBeadsDir)
	if err != nil || len(routes) == 0 {
		return res
	}

	townAgents, err := beads.NewRigLocal(townBeadsDir).ListAgentBeads()
	if err != nil {
		res.Status = StatusWarning
		res.Message = "Could not list town agent beads: " + err.Error()
		return res
	}

	for _, route := range routes {
		if route.Path == "." || strings.HasPrefix(route.Prefix, "hq-") {
			continue
		}
		rigName := strings.Split(route.Path, "/")[0]
		if ctx.RigName != "" && rigName != ctx.RigName {
			continue
		}
		rigDir := ctx.TownRoot + "/" + route.Path
		rigAgents, rigErr := beads.NewRigLocal(rigDir).ListAgentBeads()
		for id, townIssue := range townAgents {
			if !strings.HasPrefix(id, route.Prefix) {
				continue
			}
			line := id + " exists in town DB (updated " + townIssue.UpdatedAt + ")"
			if rigErr != nil {
				line += "; rig DB unreadable: " + rigErr.Error()
			} else if rigIssue, ok := rigAgents[id]; ok {
				diff := shadowedAgentFields(rigIssue, townIssue)
				line += fmt.Sprintf("; rig row updated %s; differing fields: %s", rigIssue.UpdatedAt, strings.Join(diff, ","))
				if len(diff) == 0 {
					line += "(none)"
				}
			} else {
				line += "; NO rig row (run gt doctor --fix agent-beads-exist first)"
			}
			res.Details = append(res.Details, line)
		}
	}

	sort.Strings(res.Details)
	if len(res.Details) > 0 {
		res.Status = StatusWarning
		res.Message = fmt.Sprintf("%d rig agent bead(s) shadowed by a legacy town row", len(res.Details))
		res.FixHint = "For each ID: gt polecat identity reconcile <rig>/<name> (dry-run), review, then --apply"
	}
	return res
}

// shadowedAgentFields returns the agent description fields whose values
// differ between the rig row and the town row, in a fixed order.
func shadowedAgentFields(rig, town *beads.Issue) []string {
	r, tn := beads.ParseAgentFields(rig.Description), beads.ParseAgentFields(town.Description)
	if r == nil || tn == nil {
		return []string{"description"}
	}
	var out []string
	add := func(name string, a, b string) {
		if a != b {
			out = append(out, name)
		}
	}
	add("agent_state", r.AgentState, tn.AgentState)
	add("hook_bead", r.HookBead, tn.HookBead)
	add("cleanup_status", r.CleanupStatus, tn.CleanupStatus)
	add("active_mr", r.ActiveMR, tn.ActiveMR)
	add("exit_type", r.ExitType, tn.ExitType)
	add("mr_id", r.MRID, tn.MRID)
	add("branch", r.Branch, tn.Branch)
	add("last_source_issue", r.LastSourceIssue, tn.LastSourceIssue)
	add("completion_time", r.CompletionTime, tn.CompletionTime)
	return out
}

// Fix always errors — this check is detection-only. Repair is a deliberate,
// per-ID operation (`gt polecat identity reconcile`), never a patrol sweep.
func (c *AgentBeadsShadowCheck) Fix(_ *CheckContext) error {
	return fmt.Errorf("agent-beads-shadow has no auto-fix; run gt polecat identity reconcile <rig>/<name>")
}

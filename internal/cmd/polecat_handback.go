package cmd

import (
	"github.com/steveyegge/gastown/internal/beads"
)

// handedBackHook reports whether a seat's hook_bead names a bead that was
// handed back: the seat is paused and the bead is open with no assignee, which
// is what an overrun stop or an escalation close leaves behind.
//
// Only the paused state qualifies. A starting seat carries the same hook_bead
// from spawn while the bead is still open and unassigned until the sling claim,
// so a rule on the bead alone would let a nuke clear a seat that is starting.
// Any other state, a nil bead, or a bead that is hooked, in progress or
// assigned answers false.
func handedBackHook(agentState string, hook *beads.Issue) bool {
	if hook == nil {
		return false
	}
	return beads.AgentState(agentState) == beads.AgentStatePaused &&
		beads.IssueStatus(hook.Status) == beads.StatusOpen &&
		hook.Assignee == ""
}

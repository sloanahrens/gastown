package polecat

import (
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// SpawnGrace reports whether a polecat whose work is assigned but whose session
// is not up yet should still be read as spawning rather than stalled.
//
// gt sling writes agent_state=spawning into the agent bead before the sandbox
// and session exist; the first heartbeat moves the bead on to working. Query
// paths that synthesize "stalled" from "work assigned, no live session" would
// otherwise call that polecat stalled within seconds of dispatch, and the
// restart paths would chase a session that is still booting (gt-yteq). The
// window is the Witness's own startup grace
// (config.WitnessThresholds.HeartbeatStartupGrace, default 5m), so list,
// capacity, manager and witness agree on when spawning ends.
//
// Only an explicit spawning state earns the grace. A bead that already says
// working (or idle) with a dead session is a crash mid-work, which is what
// stalled means — and a spawning bead whose last write is older than the window
// is a dispatch that never came up, which is a real stall.
//
// Missing facts fail toward the existing stalled verdict rather than inventing
// grace: a zero window, a bead with no update timestamp, or no clock all return
// false.
func SpawnGrace(agentState string, updatedAt, now time.Time, grace time.Duration) bool {
	if !beads.AgentState(strings.TrimSpace(agentState)).IsSpawning() {
		return false
	}
	if grace <= 0 || updatedAt.IsZero() || now.IsZero() {
		return false
	}
	return now.Sub(updatedAt) < grace
}

// sessionDownState is the state for assigned work with no live session: still
// spawning inside the grace window, stalled after it. Every "session dead, work
// assigned" site must build its state through this — a grace applied at one
// site and hard-coded away at another reads the same polecat two ways
// (gt-2540, gt-yteq).
func sessionDownState(spawning bool) State {
	if spawning {
		return StateSpawning
	}
	return StateStalled
}

// AgentBeadUpdatedAt returns an agent bead's last write, or the zero time when
// the bead is missing or carries no parseable timestamp. SpawnGrace reads a
// zero time as "no grace", so a bead nobody can date fails toward stalled.
func AgentBeadUpdatedAt(issue *beads.Issue) time.Time {
	if issue == nil {
		return time.Time{}
	}
	return beads.ParseIssueTime(issue.UpdatedAt)
}

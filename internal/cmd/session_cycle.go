package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Role-scoped session cycling: the pieces a patrol role needs to respawn its
// OWN session in place, with no handoff mail, at the end of a unit of work.
//
// The refinery (completeUnitAndCycle, after each landed merge) and the witness
// (maybeCycleWitnessSession, at a quiet patrol boundary) share them. A third
// role (the deacon, claude-9jq tier 4) reuses them the same way:
//
//  1. decide every gate first; a skip prints "○ session kept: <cause>" and
//     leaves the session and its patrol wisp exactly as they were;
//  2. guard the caller with ownPaneCallerMismatch (GT_ROLE, TMUX_PANE, the
//     pane's real session): from any other caller a respawn would kill the
//     caller's own pane;
//  3. skip, never sleep, inside the handoff cooldown (handoffCooldownCause);
//  4. persist whatever the successor needs, then recordOwnSessionCycle
//     (cooldown stamp, handoff marker with reason "unit-cycle", town log);
//  5. respawnOwnSessionFresh LAST: it kills this process.
//
// Queued nudges must survive step 5, so a caller that closes its patrol right
// before respawning passes drainNudges=false to runPatrolReportFor.

// unitCycleHandoffReason is the handoff-marker reason every role-scoped cycle
// writes; gt prime reads it back as primeHandoffReason.
const unitCycleHandoffReason = "unit-cycle"

// ownPaneCallerMismatch returns why the caller is not wantRole running in its
// own pane of wantSession, or "" when it is. getenv and paneSession are seams
// so tests never touch the real environment or tmux.
func ownPaneCallerMismatch(wantRole, wantSession string, getenv func(string) string, paneSession func() (string, error)) string {
	if role := getenv("GT_ROLE"); role != wantRole {
		return fmt.Sprintf("caller is not %s (GT_ROLE=%q)", wantRole, role)
	}
	if getenv("TMUX_PANE") == "" {
		return "not inside tmux"
	}
	sess, err := paneSession()
	if err != nil {
		return fmt.Sprintf("pane session unknown: %v", err)
	}
	if sess != wantSession {
		return fmt.Sprintf("pane is in %s, not %s", sess, wantSession)
	}
	return ""
}

// handoffCooldownCause returns why a cycle must be skipped because the last
// handoff was inside constants.MinHandoffCooldown, or "" when it is clear.
// next says when the skipped cycle will be retried ("the next unit cycles").
func handoffCooldownCause(handoffAge func() (time.Duration, bool), next string) string {
	if age, ok := handoffAge(); ok && age < constants.MinHandoffCooldown {
		return fmt.Sprintf("last handoff %v ago (< %v); %s",
			age.Round(time.Second), constants.MinHandoffCooldown, next)
	}
	return ""
}

// recordOwnSessionCycle records a role-scoped cycle of session, whose runtime
// files live in workDir: the cooldown timestamp, the handoff marker the
// successor's gt prime reads, and the town handoff log and feed event.
func recordOwnSessionCycle(townRoot, workDir, session string) {
	recordHandoffTimeIn(workDir)
	writeHandoffMarker(workDir, session, unitCycleHandoffReason)
	agent := sessionToGTRole(session)
	if agent == "" {
		agent = session
	}
	_ = LogHandoff(townRoot, agent, unitCycleHandoffReason)
	_ = events.LogFeed(events.TypeHandoff, agent, events.HandoffPayload(unitCycleHandoffReason, true))
}

// respawnOwnSessionFresh respawns the caller's own pane (TMUX_PANE) of
// session with a fresh (not --continue) agent. On success it does not return
// to a live caller: respawn-pane -k kills this process.
func respawnOwnSessionFresh(session string) error {
	pane := os.Getenv("TMUX_PANE")
	restartCmd, err := buildRestartCommandWithOpts(session, buildRestartCommandOpts{ContinueSession: false})
	if err != nil {
		return err
	}
	t := tmux.NewTmuxWithSocket(tmux.SocketFromEnv())
	updateSessionEnvForHandoff(t, session)
	return respawnOwnPane(t, session, pane, restartCmd)
}

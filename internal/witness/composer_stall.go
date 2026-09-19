package witness

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Composer-stall detection for the rig's refinery (gt-hkhu).
//
// The refinery is the one agent whose liveness has no heartbeat: every other
// role writes one, and a fresh heartbeat is what lets the rest of the patrol
// skip stall analysis. So for the refinery the patrol falls back to session and
// process existence — and a refinery holding a composed-but-unsubmitted
// instruction has both. It idled 78 and 100+ minutes on 2026-09-18 with queued
// input in the pane while every check reported it running and MRs aged in the
// queue behind it.
//
// This check supplies the missing signal. It is deliberately conservative: the
// submit action only fires when the pane holds unsubmitted input AND the
// session has produced no output at all for the whole frozen window, which is
// what keeps it off a working or idle-await refinery (see
// tmux.DetectComposerStall for the false positive this avoids).

// Actions reported for a detected composer stall. A stall that is still
// pending after the submit attempt is the case the patrol must escalate —
// the agent is wedged past what a keystroke can fix.
const (
	// ComposerStallActionSubmittedQueued means queued messages were flushed
	// with ctrl+x ctrl+s.
	ComposerStallActionSubmittedQueued = "submitted-queued-input"
	// ComposerStallActionSubmittedEnter means typed composer text was
	// submitted with Enter.
	ComposerStallActionSubmittedEnter = "submitted-pending-input"
	// ComposerStallActionStillPending means the input did not leave the
	// composer after the submit attempt.
	ComposerStallActionStillPending = "still-pending"
)

// composerStallRecheckDelay is the settle time before re-probing the composer
// after a submit. Var so tests can zero it.
var composerStallRecheckDelay = 750 * time.Millisecond

// composerStallRecheckAttempts is how many times to re-probe for the composer
// clearing. Claude Code needs a moment to repaint after ctrl+x ctrl+s, and a
// single early read would report a recovered agent as still stalled.
const composerStallRecheckAttempts = 3

// RefineryStallResult reports one composer-stall finding and what was done.
type RefineryStallResult struct {
	Session string
	// Agent names the role the check applies to — always "refinery" today.
	Agent string
	// StallType is the stall classification ("composer-stall").
	StallType string
	// State is the composer state observed ("pending").
	State string
	// Inactivity is how long the session had produced no output.
	Inactivity time.Duration
	// Action is what the scan did about it (see the ComposerStallAction*
	// constants).
	Action string
	// Error is set when the submit attempt failed or the input did not clear.
	Error error
}

// DetectRefineryStallResult holds the outcome of a refinery stall check.
type DetectRefineryStallResult struct {
	// Checked is 1 when a live refinery session with a live agent was
	// inspected, 0 otherwise.
	Checked int
	// Stalls holds confirmed composer stalls (at most one — there is one
	// refinery per rig).
	Stalls []RefineryStallResult
	// Errors holds transient failures (pane capture, activity lookup).
	Errors []error
}

// DetectStalledRefinery checks the rig's refinery session for a
// composed-but-unsubmitted instruction and, on a confirmed stall, submits it.
//
// It is a no-op when the refinery is not running or its agent process is dead:
// those are the daemon's and zombie detection's cases, not this one. It only
// acts on the third state — session alive, agent alive, and nothing happening.
//
// The caller owns the follow-up. A "still-pending" action means the keystroke
// did not free the composer and the session needs a restart, which is the
// operator's interim fix and stays a human/patrol decision rather than
// something this scan does silently.
func DetectStalledRefinery(workDir, rigName string) *DetectRefineryStallResult {
	result := &DetectRefineryStallResult{}

	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		townRoot = workDir
	}
	initRegistryFromTownRoot(townRoot)

	sessionName := session.RefinerySessionName(session.PrefixFor(rigName))
	t := tmux.NewTmux()

	running, err := t.HasSession(sessionName)
	if err != nil {
		result.Errors = append(result.Errors,
			fmt.Errorf("checking refinery session %s: %w", sessionName, err))
		return result
	}
	if !running {
		return result // Not running — the daemon starts the refinery on heartbeat.
	}
	result.Checked = 1

	if !t.IsAgentAlive(sessionName) {
		// Session alive, agent process dead: the daemon's respawn path.
		return result
	}

	frozenFor := config.LoadOperationalConfig(townRoot).GetWitnessConfig().ComposerStallFrozenForD()
	stall, err := t.DetectComposerStall(sessionName, frozenFor)
	if err != nil {
		result.Errors = append(result.Errors, err)
		return result
	}
	if !stall.Stalled {
		return result
	}

	item := RefineryStallResult{
		Session:    sessionName,
		Agent:      "refinery",
		StallType:  "composer-stall",
		State:      stall.State.String(),
		Inactivity: stall.Inactivity,
	}

	if err := t.SubmitPendingInput(sessionName, stall.Queued); err != nil {
		item.Action = ComposerStallActionStillPending
		item.Error = err
		result.Stalls = append(result.Stalls, item)
		return result
	}

	item.Action = composerStallActionFor(stall.Queued)
	if err := confirmComposerCleared(t, sessionName, frozenFor); err != nil {
		item.Action = ComposerStallActionStillPending
		item.Error = err
	}
	result.Stalls = append(result.Stalls, item)
	return result
}

// composerStallActionFor maps the submit mechanism that a pending state needs
// onto its reported action.
func composerStallActionFor(queued bool) string {
	if queued {
		return ComposerStallActionSubmittedQueued
	}
	return ComposerStallActionSubmittedEnter
}

// confirmComposerCleared re-probes the pane after a submit and reports an error
// if the composer still holds input. A successful submit starts a turn, which
// the probe reports as busy — either non-pending state is a pass.
func confirmComposerCleared(t *tmux.Tmux, sessionName string, frozenFor time.Duration) error {
	last := tmux.ComposerUnknown.String()
	for i := 0; i < composerStallRecheckAttempts; i++ {
		time.Sleep(composerStallRecheckDelay)
		after, err := t.DetectComposerStall(sessionName, frozenFor)
		if err != nil {
			last = err.Error()
			continue
		}
		if after.State != tmux.ComposerPending {
			return nil
		}
		last = after.State.String()
	}
	return fmt.Errorf("composer still holds input after submit (state: %s)", last)
}

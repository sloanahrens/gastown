package witness

import (
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
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
// submit action only fires when the pane holds unsubmitted input AND a clock
// has run out on it — no output at all for the whole frozen window, or the input
// observed waiting for the same window across a run that has been earned.
// Nothing here acts on pane shape, because pane shape cannot separate a wedge
// from an idle-await refinery between cycles (gt-hkhu). What the age clock adds
// is that only a run whose samples are consecutive, span a minimum window,
// belong to this tmux session, and kept the pane's transcript region unchanged
// may trip it — so an agent that did any work in the window restarts the clock
// instead of being flagged (gt-afa7; see tmux.DetectComposerStallTracked).
//
// The age clock matters here as much as in the daemon: the patrol is the
// detector that reports "0 stalls" up the chain, and its stamps are shared with
// the daemon heartbeat, so whichever probes first starts the clock.

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
	// ComposerStallActionDetectedDryRun means a stall was confirmed but the
	// caller asked for detection only — no keystrokes were sent to the live
	// session.
	ComposerStallActionDetectedDryRun = "detected-dry-run"
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
	// PendingFor is how long the composer had been continuously observed
	// holding unsubmitted input, as the shared clock reported it. It is
	// non-zero whenever an earlier probe started the run — including when the
	// silence window is what tripped this verdict, and when both clocks ran
	// out. It is zero only on the first observation of a run. Do not read it as
	// "which clock fired": a run that is not yet continuous also reports a real
	// age here, and the age alone does not say the run may be acted on (gt-afa7).
	PendingFor time.Duration
	// PendingSamples is how many consecutive observations have seen the
	// composer pending. A run needs several, spanning a minimum window, before
	// its age counts, so a large PendingFor beside a small PendingSamples means
	// the clock was restarted mid-run by progress (gt-afa7).
	PendingSamples int
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
//
// dryRun reports a confirmed stall without sending anything to the live
// session — the submit is the only side effect a patrol scan has on a running
// agent's pane, and a caller that wants detection without that mutation
// (om major on gt-wisp-q9os) sets this instead of acting on the result.
func DetectStalledRefinery(workDir, rigName string, dryRun bool) *DetectRefineryStallResult {
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
	clock := tmux.NewPendingInputClock(constants.TownRuntimePath(townRoot))
	stall, err := t.DetectComposerStallTracked(sessionName, frozenFor, clock)
	if err != nil {
		result.Errors = append(result.Errors, err)
		return result
	}
	if !stall.Stalled {
		return result
	}

	item := RefineryStallResult{
		Session:        sessionName,
		Agent:          "refinery",
		StallType:      "composer-stall",
		State:          stall.State.String(),
		Inactivity:     stall.Inactivity,
		PendingFor:     stall.PendingFor,
		PendingSamples: stall.PendingSamples,
	}

	if dryRun {
		item.Action = ComposerStallActionDetectedDryRun
		result.Stalls = append(result.Stalls, item)
		return result
	}

	if err := t.SubmitPendingInput(sessionName, stall.Queued); err != nil {
		item.Action = ComposerStallActionStillPending
		item.Error = err
		result.Stalls = append(result.Stalls, item)
		return result
	}
	// The input has been acted on, so the wait it accumulated is over.
	clock.Reset(sessionName)

	item.Action = composerStallActionFor(stall.Queued)
	if err := confirmComposerCleared(t, sessionName, frozenFor); err != nil {
		if errors.Is(err, errComposerUnverifiable) {
			// The flush could not be verified, but nothing says it failed.
			// Report that beside the submit, without claiming the composer is
			// still holding input — that action is what sends the patrol to a
			// restart.
			item.Error = err
		} else {
			item.Action = ComposerStallActionStillPending
			item.Error = err
		}
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

// errComposerUnverifiable reports that the pane could not be classified after a
// submit, so the flush could be neither confirmed nor denied. It is distinct
// from a confirmed strand: see confirmComposerCleared.
var errComposerUnverifiable = errors.New("composer could not be classified after submit")

// confirmComposerCleared re-probes the pane after a submit and reports an error
// if the composer still holds input. A successful submit starts a turn, which
// the probe reports as busy — either non-pending state is a pass.
//
// An unclassifiable pane is a third outcome, not a pass and not a failure. It
// returns errComposerUnverifiable, which callers report without treating the
// flush as failed. Folding "I could not tell" into the strand verdict would send
// the patrol to a restart on a pane that was merely mid-repaint — which is very
// likely what an unclassifiable capture is, three quarters of a second after
// ctrl+x ctrl+s (gt-afa7).
//
// A capture that fails outright (tmux itself erroring, not a classifiable-but-
// ambiguous pane) gets the same retry treatment as an unclassifiable read
// rather than an immediate strand verdict: one flaky capture three quarters of
// a second after the submit is exactly the kind of transient miss the recheck
// attempts exist to ride out, and giving up on the first one defeats the retry
// loop entirely (gt-0b4z). Only when every attempt fails to capture does it
// propagate as a genuine check failure — still distinct from
// errComposerUnverifiable, since the caller reports that case as "submitted,
// could not confirm" rather than as an error.
func confirmComposerCleared(t *tmux.Tmux, sessionName string, frozenFor time.Duration) error {
	last := tmux.ComposerUnknown.String()
	unverifiable := false
	var captureErr error
	for i := 0; i < composerStallRecheckAttempts; i++ {
		time.Sleep(composerStallRecheckDelay)
		after, err := t.DetectComposerStall(sessionName, frozenFor)
		if err != nil {
			if errors.Is(err, tmux.ErrComposerUnobservable) {
				unverifiable = true
				captureErr = nil
				last = err.Error()
				continue
			}
			// The pane could not be read at all this round. Retry like any
			// other inconclusive read — only report it if it persists across
			// every remaining attempt.
			captureErr = err
			unverifiable = false
			last = err.Error()
			continue
		}
		captureErr = nil
		if after.State != tmux.ComposerPending {
			return nil
		}
		// Positive evidence that the input is still there outranks an earlier
		// unclassifiable or uncaptured read.
		unverifiable = false
		last = after.State.String()
	}
	if captureErr != nil {
		return captureErr
	}
	if unverifiable {
		return fmt.Errorf("%w (last: %s)", errComposerUnverifiable, last)
	}
	return fmt.Errorf("composer still holds input after submit (state: %s)", last)
}

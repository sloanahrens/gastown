package cmd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
)

// nudgeTmux is the part of *tmux.Tmux nudge delivery drives.
type nudgeTmux interface {
	HasSession(name string) (bool, error)
	IsBusy(target string) bool
	WaitForIdle(session string, timeout time.Duration) error
	NudgeSessionWithOpts(session, message string, opts tmux.NudgeOpts) error
	WaitForInputConsumed(session string, window time.Duration) (tmux.InputConsumption, error)
	SessionAgentPreset(session, townRootHint string) (string, *config.AgentPresetInfo, bool)
}

var _ nudgeTmux = (*tmux.Tmux)(nil)

// nudgeDelivery delivers nudges to sessions in one mode: what `gt nudge` does
// once it has resolved the target to a session name.
type nudgeDelivery struct {
	tmux     nudgeTmux
	townRoot string
	mode     string // NudgeModeImmediate, NudgeModeQueue or NudgeModeWaitIdle
	priority string
	force    bool // immediate mode interrupts a busy target

	waitIdleTimeout time.Duration // wait-idle's wait before it queues
	watchTimeout    time.Duration // the post-queue idle watcher's window
	pollInterval    time.Duration // the idle watcher's poll interval
	probeWindow     time.Duration // the consumption probe's window
	clock           clockwork.Clock

	// startPoller starts the background nudge-poller that drains a session's
	// queue; it is idempotent (nudge.StartPoller).
	startPoller func(townRoot, session string) (int, error)
	stderr      io.Writer
}

// newNudgeDelivery is the delivery the gt nudge flags and timings describe.
func newNudgeDelivery(t nudgeTmux, townRoot string, stderr io.Writer) *nudgeDelivery {
	return &nudgeDelivery{
		tmux:            t,
		townRoot:        townRoot,
		mode:            nudgeModeFlag,
		priority:        nudgePriorityFlag,
		force:           nudgeForceFlag,
		waitIdleTimeout: waitIdleTimeout,
		watchTimeout:    idleWatcherTimeout,
		pollInterval:    idleWatcherPollInterval,
		probeWindow:     immediateTurnProbeWindow,
		clock:           clockwork.NewRealClock(),
		startPoller:     nudge.StartPoller,
		stderr:          stderr,
	}
}

func (d *nudgeDelivery) queued(sender, message string) nudge.QueuedNudge {
	return nudge.QueuedNudge{Sender: sender, Message: message, Priority: d.priority}
}

// deliver routes a nudge by mode.
// For "immediate" mode: sends directly via tmux (current behavior).
// For "queue" mode: writes to the nudge queue for cooperative delivery.
// For "wait-idle" mode: waits for idle, then delivers or falls back to queue.
func (d *nudgeDelivery) deliver(sessionName, message, sender string) error {
	// For direct tmux delivery, prefix with sender attribution.
	// Queue-based delivery stores Sender as a separate field and
	// FormatForInjection adds the prefix, so we must NOT double-prefix.
	prefixedMessage := fmt.Sprintf("[from %s] %s", sender, message)

	switch d.mode {
	case NudgeModeQueue:
		if d.townRoot == "" {
			return fmt.Errorf("--mode=queue requires a Gas Town workspace")
		}
		return nudge.Enqueue(d.townRoot, sessionName, d.queued(sender, message))

	case NudgeModeWaitIdle:
		return d.waitIdle(sessionName, message, sender)

	default: // NudgeModeImmediate
		// NudgeSessionWithOpts itself skips the Escape keystroke for agents
		// where Escape cancels in-flight generation (Claude Code — see
		// EscapeCancelsRequest / effectiveSkipEscape), so no
		// per-agent opt-in is needed here. (GH#gt-wasn, gt-cyyg)
		opts := tmux.NudgeOpts{TownRoot: d.townRoot}

		// Refuse to interrupt a busy target: immediate mode used to send
		// straight into whatever the pane was doing, and the only guard
		// against that (a busy-indicator scrape) can miss a real busy window
		// — the exact failure that turned a routine nudge into an apparent
		// operator interrupt for gastown/refinery mid a 150s `go test` run
		// (gt-cyyg). --force overrides for the genuine "break through a stuck
		// agent" case the mode exists for.
		if !d.force && d.tmux.IsBusy(sessionName) {
			fmt.Fprintf(d.stderr, "immediate: %s is busy, refusing to interrupt (use --force to override); falling back to wait-idle\n", sessionName)
			if d.townRoot == "" {
				return fmt.Errorf("--mode=immediate refused: %s is busy, and wait-idle fallback requires a Gas Town workspace; use --force to override", sessionName)
			}
			return d.waitIdle(sessionName, message, sender)
		}

		if err := d.tmux.NudgeSessionWithOpts(sessionName, prefixedMessage, opts); err != nil {
			return err
		}
		// Delivery succeeded; now check the target actually acted on it.
		if warning := d.consumptionWarning(sessionName, NudgeModeImmediate); warning != "" {
			fmt.Fprint(d.stderr, warning)
		}
		return nil
	}
}

// consumptionWarning is consumptionWarningFor over the session's own
// consumption probe and this delivery's probe window.
func (d *nudgeDelivery) consumptionWarning(sessionName, mode string) string {
	return consumptionWarningFor(d.tmux.WaitForInputConsumed, d.probeWindow, sessionName, mode)
}

// waitIdle waits for the target to become idle (prompt visible), then
// delivers directly. Falls back to queue on timeout or unverified submit. If
// both idle-wait and queue fail, falls back to immediate delivery as a last
// resort. Shared by NudgeModeWaitIdle and the busy-refusal fallback in
// NudgeModeImmediate (gt-cyyg).
func (d *nudgeDelivery) waitIdle(sessionName, message, sender string) error {
	townRoot := d.townRoot
	if townRoot == "" {
		// wait-idle needs workspace for queue fallback — fail explicitly
		// rather than silently degrading to immediate (destructive) delivery.
		return fmt.Errorf("--mode=wait-idle requires a Gas Town workspace")
	}
	// Check if the target agent supports prompt-based idle detection.
	// WaitForIdle uses Claude Code's prompt pattern (❯) and status bar (⏵⏵).
	// An agent preset without a ReadyPromptPrefix (or an unidentified one)
	// makes WaitForIdle produce false positives — it sees no busy indicator
	// and matches stale prompt characters in the pane buffer. (GH#gt-5ey3)
	// Degrade to queue mode for agents without prompt-based detection.
	if agentName, preset, ok := d.tmux.SessionAgentPreset(sessionName, townRoot); agentName != "" {
		if !ok || preset.ReadyPromptPrefix == "" {
			fmt.Fprintf(d.stderr, "wait-idle: %s agent %q has no prompt detection, using queue mode\n", sessionName, agentName)
			if qErr := nudge.Enqueue(townRoot, sessionName, d.queued(sender, message)); qErr != nil {
				formatted := nudge.FormatForInjection([]nudge.QueuedNudge{d.queued(sender, message)})
				return d.tmux.NudgeSessionWithOpts(sessionName, formatted, tmux.NudgeOpts{TownRoot: townRoot})
			}
			// Ensure a nudge-poller is running so the queue actually drains.
			// The poller is normally started by gt crew start, but if the
			// session was started manually (or the poller crashed), queued
			// nudges sit undelivered forever. StartPoller is idempotent —
			// it no-ops if a poller is already alive for this session.
			if _, pollerErr := d.startPoller(townRoot, sessionName); pollerErr != nil {
				fmt.Fprintf(d.stderr, "wait-idle: could not start nudge poller for %s: %v\n", sessionName, pollerErr)
			}
			return nil
		}
	}
	// Try to wait for idle
	err := d.tmux.WaitForIdle(sessionName, d.waitIdleTimeout)
	if err == nil {
		// Agent is idle — deliver directly. Format as system-reminder
		// so the agent processes it as a background notification rather
		// than a user interruption/correction.
		formatted := nudge.FormatForInjection([]nudge.QueuedNudge{d.queued(sender, message)})
		deliverErr := d.tmux.NudgeSessionWithOpts(sessionName, formatted, tmux.NudgeOpts{TownRoot: townRoot})
		if !errors.Is(deliverErr, tmux.ErrSubmitNotVerified) {
			if deliverErr == nil {
				// Delivery reported success because the target read as idle
				// and took the keystrokes — but "took the keystrokes" and
				// "acted on them" are different claims (gt-8hi4w, the same
				// gap immediate mode closed for gt-eigw). Warn rather than
				// let a false idle-read report a silent success.
				if warning := d.consumptionWarning(sessionName, NudgeModeWaitIdle); warning != "" {
					fmt.Fprint(d.stderr, warning)
				}
			}
			return deliverErr
		}
		fmt.Fprintf(d.stderr, "wait-idle: %v; queueing for %s\n", deliverErr, sessionName)
		if qErr := nudge.Enqueue(townRoot, sessionName, d.queued(sender, message)); qErr != nil {
			return fmt.Errorf("queue fallback after unverified submit failed: %v (original: %w)", qErr, deliverErr)
		}
		return nil
	}
	// Terminal errors (session gone, no server) — propagate, don't queue.
	// Queueing a nudge for a dead session means it will never be delivered.
	if errors.Is(err, tmux.ErrSessionNotFound) || errors.Is(err, tmux.ErrNoServer) {
		return fmt.Errorf("wait-idle: %w", err)
	}
	// Timeout (agent busy) — queue instead
	if qErr := nudge.Enqueue(townRoot, sessionName, d.queued(sender, message)); qErr != nil {
		// Queue failed — fall back to immediate as last resort.
		// Better to interrupt than lose the message entirely.
		fmt.Fprintf(d.stderr, "Warning: queue fallback failed (%v), delivering immediately\n", qErr)
		// Still use FormatForInjection so the agent sees a consistent
		// <system-reminder> format regardless of delivery path.
		formatted := nudge.FormatForInjection([]nudge.QueuedNudge{d.queued(sender, message)})
		return d.tmux.NudgeSessionWithOpts(sessionName, formatted, tmux.NudgeOpts{TownRoot: townRoot})
	}
	// Ensure a nudge-poller is running so the queue still drains after this
	// process exits. watch below only watches synchronously for
	// watchTimeout; a session that stays busy longer than that (a
	// long-running patrol turn, say) needs the background poller to pick the
	// queue back up. The poller is normally started at session launch, but if
	// it crashed or was never started for this session, nudges would queue up
	// and never be delivered (gt-9le0e). StartPoller is idempotent — it
	// no-ops if a poller is already alive for this session.
	if _, pollerErr := d.startPoller(townRoot, sessionName); pollerErr != nil {
		fmt.Fprintf(d.stderr, "wait-idle: could not start nudge poller for %s: %v\n", sessionName, pollerErr)
	}
	// Run watcher synchronously: polls for idle over a longer window.
	// The UserPromptSubmit hook drains the queue on agent input, but an
	// idle agent receives no input — so queued nudges are lost without
	// this watcher. It exits on: delivery, session death, or timeout.
	// Must be synchronous (not a goroutine) because gt nudge is a CLI
	// command — the process exits after return, killing any goroutines.
	d.watch(sessionName)
	return nil
}

// watch polls a session for idle state over watchTimeout.
// When the agent becomes idle, it drains the nudge queue and sends the
// formatted content directly via NudgeSession. This bypasses the
// UserPromptSubmit hook entirely — that hook does not fire for tmux
// send-keys input, so we cannot rely on it.
//
// This runs synchronously — gt nudge blocks until the watcher exits.
// Errors are logged to stderr rather than returned since delivery failure
// after successful queue write is non-fatal (queue persists for next drain).
//
// Exit conditions:
//   - Agent becomes idle: drain queue and deliver formatted content, exit.
//   - Queue is empty (someone else drained it): exit.
//   - Session disappears: exit (nothing to deliver to).
//   - Timeout: exit (queue stays for next input or watcher cycle).
func (d *nudgeDelivery) watch(sessionName string) {
	townRoot, timeout, interval := d.townRoot, d.watchTimeout, d.pollInterval
	fmt.Fprintf(d.stderr, "Watching %s for idle (up to %s)...\n", sessionName, timeout)
	deadline := d.clock.Now().Add(timeout)
	for d.clock.Now().Before(deadline) {
		d.clock.Sleep(interval)

		// If queue is already empty, someone else drained it.
		if nudge.QueueLen(townRoot, sessionName) == 0 {
			return
		}

		// Check if session still exists — no point watching a dead session.
		if exists, _ := d.tmux.HasSession(sessionName); !exists {
			return
		}

		// Use WaitForIdle with a short timeout instead of single-snapshot
		// IsIdle to get the consecutive-poll guard (2 polls 200ms apart).
		// This avoids false positives during inter-tool-call gaps where
		// the prompt briefly appears while Claude Code is still working.
		if err := d.tmux.WaitForIdle(sessionName, interval); err == nil {
			// Drain atomically claims queued entries (rename-based).
			// If another process raced and drained first, we get an
			// empty slice and skip delivery to avoid duplicates.
			drained, _ := nudge.Drain(townRoot, sessionName)
			if len(drained) == 0 {
				return
			}
			formatted := nudge.FormatForInjection(drained)
			if err := d.tmux.NudgeSessionWithOpts(sessionName, formatted, tmux.NudgeOpts{TownRoot: townRoot}); err != nil {
				fmt.Fprintf(d.stderr, "idle-watcher: delivery for %s failed: %v\n", sessionName, err)
				if err := nudge.Requeue(townRoot, sessionName, drained); err != nil {
					fmt.Fprintf(d.stderr, "idle-watcher: requeue for %s failed: %v\n", sessionName, err)
				}
			} else if warning := d.consumptionWarning(sessionName, NudgeModeWaitIdle); warning != "" {
				// Same false-idle gap as the direct-delivery path above: the
				// watcher's own WaitForIdle read the target as idle, but that
				// does not mean the target acted on what it was just handed.
				fmt.Fprint(d.stderr, "idle-watcher: "+warning)
			}
			return
		}
	}
	// Timeout — the nudge stays queued for the next watcher or a manual drain.
	// Say so rather than exit like the success path above (gt-z4gs).
	fmt.Fprintf(d.stderr, "idle-watcher: gave up waiting for %s to go idle after %s; nudge stays queued for the next watcher or a manual drain\n", sessionName, timeout)
}

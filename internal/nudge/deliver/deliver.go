// Package deliver is how a nudge reaches an agent's session: `gt nudge`'s
// delivery modes (wait-idle, queue, immediate), its target resolution, DND
// check and sender attribution, outside internal/cmd so the daemon's Notifier
// can deliver a nudge in-process instead of running `gt nudge`.
package deliver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Delivery modes.
const (
	// ModeImmediate sends directly via tmux send-keys.
	// This interrupts in-flight work but guarantees immediate delivery.
	ModeImmediate = "immediate"
	// ModeQueue writes to a file queue; agent picks up via hook at next
	// turn boundary. Zero interruption but delivery depends on agent turn frequency.
	ModeQueue = "queue"
	// ModeWaitIdle waits for the agent to become idle (prompt visible),
	// then delivers directly. Falls back to queue on timeout. Best of both worlds.
	ModeWaitIdle = "wait-idle"
)

// Default timings, gt nudge's.
const (
	// WaitIdleTimeout is how long wait-idle polls before falling back to queue.
	WaitIdleTimeout = 15 * time.Second
	// WatchTimeout is how long the idle watcher polls after queuing a nudge.
	// If the agent becomes idle within this window, the watcher drains the
	// queue and delivers directly. This covers the gap where an agent
	// finishes work after WaitForIdle's timeout but before anyone sends new
	// input (so UserPromptSubmit never fires and the queue never drains).
	WatchTimeout = 60 * time.Second
	// PollInterval is how often the idle watcher checks for idle.
	PollInterval = 1 * time.Second
	// ProbeWindow is how long a direct delivery watches the pane for a
	// reaction before reporting that the target did not start a turn.
	ProbeWindow = 3 * time.Second
)

// Tmux is the part of *tmux.Tmux nudge delivery drives.
type Tmux interface {
	HasSession(name string) (bool, error)
	ListSessions() ([]string, error)
	IsBusy(target string) bool
	WaitForIdle(session string, timeout time.Duration) error
	NudgeSessionWithOpts(session, message string, opts tmux.NudgeOpts) error
	WaitForInputConsumed(session string, window time.Duration) (tmux.InputConsumption, error)
	SessionAgentPreset(session, townRootHint string) (string, *config.AgentPresetInfo, bool)
}

var _ Tmux = (*tmux.Tmux)(nil)

// Delivery delivers nudges to sessions in one mode: what `gt nudge` does once
// it has resolved the target to a session name.
//
// Every wait is bounded by the context as well as by its own timeout, and the
// context is checked before every keystroke, queue write and poller start, so
// a delivery whose context ends stops where it is and reports the context's
// error: what killing the `gt nudge` process on the deadline did.
type Delivery struct {
	Tmux     Tmux
	TownRoot string
	Mode     string // ModeImmediate, ModeQueue or ModeWaitIdle
	Priority string // nudge.PriorityNormal or nudge.PriorityUrgent
	Force    bool   // immediate mode interrupts a busy target

	WaitIdleTimeout time.Duration // wait-idle's wait before it queues
	WatchTimeout    time.Duration // the post-queue idle watcher's window
	PollInterval    time.Duration // the idle watcher's poll interval
	ProbeWindow     time.Duration // the consumption probe's window
	Clock           clockwork.Clock

	// StartPoller starts the background nudge-poller that drains a session's
	// queue; it is idempotent (nudge.StartPoller).
	StartPoller func(townRoot, session string) (int, error)
	// Stderr receives the progress and warning lines gt nudge prints.
	Stderr io.Writer
}

// New is a wait-idle delivery into townRoot on t with gt nudge's timings,
// normal priority, the real poller and the real clock, printing nothing.
func New(t Tmux, townRoot string) *Delivery {
	return &Delivery{
		Tmux:            t,
		TownRoot:        townRoot,
		Mode:            ModeWaitIdle,
		Priority:        nudge.PriorityNormal,
		WaitIdleTimeout: WaitIdleTimeout,
		WatchTimeout:    WatchTimeout,
		PollInterval:    PollInterval,
		ProbeWindow:     ProbeWindow,
		Clock:           clockwork.NewRealClock(),
		StartPoller:     nudge.StartPoller,
		Stderr:          io.Discard,
	}
}

func (d *Delivery) queued(sender, message string) nudge.QueuedNudge {
	return nudge.QueuedNudge{Sender: sender, Message: message, Priority: d.Priority}
}

// waitForIdle is Tmux.WaitForIdle bounded by ctx as well as by timeout. It
// reports ctx's error when ctx ended during the wait, and
// context.DeadlineExceeded when a wait cut short to ctx's deadline failed,
// whatever the wait said: either way the deadline, not the target, ended it.
func (d *Delivery) waitForIdle(ctx context.Context, sessionName string, timeout time.Duration) error {
	cut := false
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < timeout {
			timeout, cut = max(left, 0), true
		}
	}
	err := d.Tmux.WaitForIdle(sessionName, timeout)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil && cut {
		return context.DeadlineExceeded
	}
	return err
}

// Deliver routes a nudge by mode.
// For "immediate" mode: sends directly via tmux.
// For "queue" mode: writes to the nudge queue for cooperative delivery.
// For "wait-idle" mode: waits for idle, then delivers or falls back to queue.
func (d *Delivery) Deliver(ctx context.Context, sessionName, message, sender string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// For direct tmux delivery, prefix with sender attribution.
	// Queue-based delivery stores Sender as a separate field and
	// FormatForInjection adds the prefix, so we must NOT double-prefix.
	prefixedMessage := fmt.Sprintf("[from %s] %s", sender, message)

	switch d.Mode {
	case ModeQueue:
		if d.TownRoot == "" {
			return fmt.Errorf("--mode=queue requires a Gas Town workspace")
		}
		return nudge.Enqueue(d.TownRoot, sessionName, d.queued(sender, message))

	case ModeWaitIdle:
		return d.waitIdle(ctx, sessionName, message, sender)

	default: // ModeImmediate
		// NudgeSessionWithOpts itself skips the Escape keystroke for agents
		// where Escape cancels in-flight generation (Claude Code — see
		// EscapeCancelsRequest / effectiveSkipEscape), so no
		// per-agent opt-in is needed here. (GH#gt-wasn, gt-cyyg)
		opts := tmux.NudgeOpts{TownRoot: d.TownRoot}

		// Refuse to interrupt a busy target: immediate mode used to send
		// straight into whatever the pane was doing, and the only guard
		// against that (a busy-indicator scrape) can miss a real busy window
		// — the exact failure that turned a routine nudge into an apparent
		// operator interrupt for gastown/refinery mid a 150s `go test` run
		// (gt-cyyg). --force overrides for the genuine "break through a stuck
		// agent" case the mode exists for.
		if !d.Force && d.Tmux.IsBusy(sessionName) {
			fmt.Fprintf(d.Stderr, "immediate: %s is busy, refusing to interrupt (use --force to override); falling back to wait-idle\n", sessionName)
			if d.TownRoot == "" {
				return fmt.Errorf("--mode=immediate refused: %s is busy, and wait-idle fallback requires a Gas Town workspace; use --force to override", sessionName)
			}
			return d.waitIdle(ctx, sessionName, message, sender)
		}

		if err := d.Tmux.NudgeSessionWithOpts(sessionName, prefixedMessage, opts); err != nil {
			return err
		}
		// Delivery succeeded; now check the target actually acted on it.
		if warning := d.consumptionWarning(sessionName, ModeImmediate); warning != "" {
			fmt.Fprintf(d.Stderr, "%s", warning)
		}
		return nil
	}
}

func (d *Delivery) consumptionWarning(sessionName, mode string) string {
	return ConsumptionWarning(d.Tmux.WaitForInputConsumed, d.ProbeWindow, sessionName, mode)
}

// ConsumptionWarning watches a target, through probe for window, for a
// reaction to a nudge that mode has just delivered directly (immediate, or
// wait-idle's own direct delivery once the target went idle), and returns a
// warning line when the target took the input without acting on it — or when
// consumption could not be established (an unreadable pane, or a frozen pane
// with nothing to date it by, both read UNKNOWN and name a re-probe instead of
// claiming a wedge, gt-7xnv). It returns "" only on a positive, unambiguous
// verdict.
//
// This exists because "the pty took the keystrokes" and "the agent acted on
// them" are different claims, and only the first was ever being verified
// (gt-eigw): on 2026-09-09 be-refinery accepted an immediate nudge, an Enter
// keystroke and mail, started no turn for any of them, and sat idle for ~15
// minutes while the daemon logged "Refinery for beads already running,
// skipping spawn" on every heartbeat. Every delivery had reported success.
//
// wait-idle's direct-delivery path had the same gap (gt-8hi4w): a target that
// WaitForIdle read as idle — a stale prompt, a frozen pane — could take the
// keystrokes without starting a turn, and 'gt nudge' printed '✓ Nudged ...
// (wait-idle)' regardless. mode is only used to label the warning; the probe
// itself does not care how the text was delivered.
//
// It reports rather than fails. Delivery genuinely succeeded — the text left
// the composer — and the target may still act on it later, so a non-zero exit
// here would make every caller (patrols, slings, operators) treat a delivered
// nudge as an undelivered one. The warning is the signal; the recovery stays
// the operator's or the patrol's, per tmux.SubmitPendingInput's contract.
func ConsumptionWarning(probe func(sessionName string, window time.Duration) (tmux.InputConsumption, error), window time.Duration, sessionName, mode string) string {
	verdict, err := probe(sessionName, window)
	if err != nil {
		// Fail closed (gt-7xnv): an unobservable pane says nothing about the
		// nudge, so it must not read as one that was consumed — but it must not
		// claim a wedge either, so the word here is UNKNOWN, not "started no
		// turn". Re-probe before acting on it.
		return fmt.Sprintf(
			"%s: %s took the nudge but consumption is UNKNOWN — the probe could not read the pane (%v). "+
				"Re-check with 'gt status' before acting.\n",
			mode, sessionName, err)
	}
	if verdict == tmux.InputConsumptionUndated {
		// The pane was frozen and holding input but has no content above the
		// input box to date it by (gt-7xnv): the nudge may be the input, or
		// any older text. Not a strand claim — re-probe on a longer window.
		return fmt.Sprintf(
			"%s: %s still holds input and the pane was frozen for %s, but it has nothing above "+
				"the input box to date it by — consumption is UNKNOWN (UNDATED), not a strand. "+
				"Re-check with 'gt status' before acting.\n",
			mode, sessionName, window)
	}
	if verdict != tmux.InputConsumptionNotConsumed {
		return ""
	}
	return fmt.Sprintf(
		"%s: %s accepted the nudge but started no turn within %s — its input is "+
			"still stranded in the composer/queue, which is how a wedged session presents "+
			"(gt-eigw). Inspect it with 'gt status'; if it stays stuck, restart "+
			"that session.\n",
		mode, sessionName, window)
}

// waitIdle waits for the target to become idle (prompt visible), then
// delivers directly. Falls back to queue on timeout or unverified submit. If
// both idle-wait and queue fail, falls back to immediate delivery as a last
// resort. Shared by ModeWaitIdle and the busy-refusal fallback in
// ModeImmediate (gt-cyyg).
func (d *Delivery) waitIdle(ctx context.Context, sessionName, message, sender string) error {
	townRoot := d.TownRoot
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
	if agentName, preset, ok := d.Tmux.SessionAgentPreset(sessionName, townRoot); agentName != "" {
		if !ok || preset.ReadyPromptPrefix == "" {
			if err := ctx.Err(); err != nil {
				return err
			}
			fmt.Fprintf(d.Stderr, "wait-idle: %s agent %q has no prompt detection, using queue mode\n", sessionName, agentName)
			if qErr := nudge.Enqueue(townRoot, sessionName, d.queued(sender, message)); qErr != nil {
				formatted := nudge.FormatForInjection([]nudge.QueuedNudge{d.queued(sender, message)})
				return d.Tmux.NudgeSessionWithOpts(sessionName, formatted, tmux.NudgeOpts{TownRoot: townRoot})
			}
			// Ensure a nudge-poller is running so the queue actually drains.
			// The poller is normally started by gt crew start, but if the
			// session was started manually (or the poller crashed), queued
			// nudges sit undelivered forever. StartPoller is idempotent —
			// it no-ops if a poller is already alive for this session.
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, pollerErr := d.StartPoller(townRoot, sessionName); pollerErr != nil {
				fmt.Fprintf(d.Stderr, "wait-idle: could not start nudge poller for %s: %v\n", sessionName, pollerErr)
			}
			return nil
		}
	}
	// Try to wait for idle
	err := d.waitForIdle(ctx, sessionName, d.WaitIdleTimeout)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	if err == nil {
		// Agent is idle — deliver directly. Format as system-reminder
		// so the agent processes it as a background notification rather
		// than a user interruption/correction.
		formatted := nudge.FormatForInjection([]nudge.QueuedNudge{d.queued(sender, message)})
		deliverErr := d.Tmux.NudgeSessionWithOpts(sessionName, formatted, tmux.NudgeOpts{TownRoot: townRoot})
		if !errors.Is(deliverErr, tmux.ErrSubmitNotVerified) {
			if deliverErr == nil {
				// Delivery reported success because the target read as idle
				// and took the keystrokes — but "took the keystrokes" and
				// "acted on them" are different claims (gt-8hi4w, the same
				// gap immediate mode closed for gt-eigw). Warn rather than
				// let a false idle-read report a silent success.
				if warning := d.consumptionWarning(sessionName, ModeWaitIdle); warning != "" {
					fmt.Fprintf(d.Stderr, "%s", warning)
				}
			}
			return deliverErr
		}
		fmt.Fprintf(d.Stderr, "wait-idle: %v; queueing for %s\n", deliverErr, sessionName)
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
		fmt.Fprintf(d.Stderr, "Warning: queue fallback failed (%v), delivering immediately\n", qErr)
		// Still use FormatForInjection so the agent sees a consistent
		// <system-reminder> format regardless of delivery path.
		formatted := nudge.FormatForInjection([]nudge.QueuedNudge{d.queued(sender, message)})
		return d.Tmux.NudgeSessionWithOpts(sessionName, formatted, tmux.NudgeOpts{TownRoot: townRoot})
	}
	// Ensure a nudge-poller is running so the queue still drains after this
	// returns. Watch below only watches synchronously for WatchTimeout; a
	// session that stays busy longer than that (a long-running patrol turn,
	// say) needs the background poller to pick the queue back up. The poller
	// is normally started at session launch, but if it crashed or was never
	// started for this session, nudges would queue up and never be delivered
	// (gt-9le0e). StartPoller is idempotent — it no-ops if a poller is
	// already alive for this session.
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, pollerErr := d.StartPoller(townRoot, sessionName); pollerErr != nil {
		fmt.Fprintf(d.Stderr, "wait-idle: could not start nudge poller for %s: %v\n", sessionName, pollerErr)
	}
	// Run watcher synchronously: polls for idle over a longer window.
	// The UserPromptSubmit hook drains the queue on agent input, but an
	// idle agent receives no input — so queued nudges are lost without
	// this watcher. It exits on: delivery, session death, or timeout.
	// Must be synchronous (not a goroutine) because gt nudge is a CLI
	// command — the process exits after return, killing any goroutines.
	d.Watch(ctx, sessionName)
	return ctx.Err()
}

// Watch polls a session for idle state over WatchTimeout.
// When the agent becomes idle, it drains the nudge queue and sends the
// formatted content directly via NudgeSession. This bypasses the
// UserPromptSubmit hook entirely — that hook does not fire for tmux
// send-keys input, so we cannot rely on it.
//
// This runs synchronously — gt nudge blocks until the watcher exits.
// Errors are logged to Stderr rather than returned since delivery failure
// after successful queue write is non-fatal (queue persists for next drain).
//
// Exit conditions:
//   - Agent becomes idle: drain queue and deliver formatted content, exit.
//   - Queue is empty (someone else drained it): exit.
//   - Session disappears: exit (nothing to deliver to).
//   - ctx ends: exit (the nudge stays queued).
//   - Timeout: exit (queue stays for next input or watcher cycle).
func (d *Delivery) Watch(ctx context.Context, sessionName string) {
	townRoot, timeout, interval := d.TownRoot, d.WatchTimeout, d.PollInterval
	fmt.Fprintf(d.Stderr, "Watching %s for idle (up to %s)...\n", sessionName, timeout)
	deadline := d.Clock.Now().Add(timeout)
	for d.Clock.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-d.Clock.After(interval):
		}

		// If queue is already empty, someone else drained it.
		if nudge.QueueLen(townRoot, sessionName) == 0 {
			return
		}

		// Check if session still exists — no point watching a dead session.
		if exists, _ := d.Tmux.HasSession(sessionName); !exists {
			return
		}

		// Use WaitForIdle with a short timeout instead of single-snapshot
		// IsIdle to get the consecutive-poll guard (2 polls 200ms apart).
		// This avoids false positives during inter-tool-call gaps where
		// the prompt briefly appears while Claude Code is still working.
		if err := d.waitForIdle(ctx, sessionName, interval); err == nil {
			// Drain atomically claims queued entries (rename-based).
			// If another process raced and drained first, we get an
			// empty slice and skip delivery to avoid duplicates.
			drained, _ := nudge.Drain(townRoot, sessionName)
			if len(drained) == 0 {
				return
			}
			formatted := nudge.FormatForInjection(drained)
			if err := d.Tmux.NudgeSessionWithOpts(sessionName, formatted, tmux.NudgeOpts{TownRoot: townRoot}); err != nil {
				fmt.Fprintf(d.Stderr, "idle-watcher: delivery for %s failed: %v\n", sessionName, err)
				Requeue(d.Stderr, townRoot, sessionName, "idle-watcher", drained)
			} else if warning := d.consumptionWarning(sessionName, ModeWaitIdle); warning != "" {
				// Same false-idle gap as the direct-delivery path above: the
				// watcher's own WaitForIdle read the target as idle, but that
				// does not mean the target acted on what it was just handed.
				fmt.Fprintf(d.Stderr, "idle-watcher: %s", warning)
			}
			return
		}
	}
	// Timeout — the nudge stays queued for the next watcher or a manual drain.
	// Say so rather than exit like the success path above (gt-z4gs).
	fmt.Fprintf(d.Stderr, "idle-watcher: gave up waiting for %s to go idle after %s; nudge stays queued for the next watcher or a manual drain\n", sessionName, timeout)
}

// Requeue puts nudges whose delivery failed back on sessionName's queue,
// reporting a failure to stderr under source.
func Requeue(stderr io.Writer, townRoot, sessionName, source string, drained []nudge.QueuedNudge) {
	if err := nudge.Requeue(townRoot, sessionName, drained); err != nil {
		fmt.Fprintf(stderr, "%s: requeue for %s failed: %v\n", source, sessionName, err)
	}
}

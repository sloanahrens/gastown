package cmd

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	nudgePollerIntervalFlag string
	nudgePollerIdleFlag     string
)

func init() {
	rootCmd.AddCommand(nudgePollerCmd)
	nudgePollerCmd.Flags().StringVar(&nudgePollerIntervalFlag, "interval", nudge.DefaultPollInterval, "Poll interval (e.g., 10s, 30s)")
	nudgePollerCmd.Flags().StringVar(&nudgePollerIdleFlag, "idle-timeout", nudge.DefaultIdleTimeout, "How long to wait for agent idle before skipping")
}

var nudgePollerCmd = &cobra.Command{
	Use:    "nudge-poller <session>",
	Short:  "Background nudge queue poller",
	Hidden: true, // Internal command — launched by crew manager, not by users.
	Long: `Polls the nudge queue for a tmux session and drains it when the agent
is idle. This is the background equivalent of Claude's UserPromptSubmit hook
drain — it ensures queued nudges reach an agent that stays in one long turn
and so never reaches a turn boundary (gt-9le0e).

This command runs as a long-lived background process. It exits when:
  - The target tmux session dies
  - It receives SIGTERM (from StopPoller or session teardown)
  - The poll loop encounters an unrecoverable error

Normally launched automatically by 'gt crew start' for non-Claude agents.
Not intended for direct user invocation.`,
	Args: cobra.ExactArgs(1),
	RunE: runNudgePoller,
}

func runNudgePoller(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("cannot find town root: %w", err)
	}

	pollInterval, err := time.ParseDuration(nudgePollerIntervalFlag)
	if err != nil {
		return fmt.Errorf("invalid --interval: %w", err)
	}

	idleTimeout, err := time.ParseDuration(nudgePollerIdleFlag)
	if err != nil {
		return fmt.Errorf("invalid --idle-timeout: %w", err)
	}

	t := tmux.NewTmux()

	// Verify session exists before starting the loop.
	if exists, _ := t.HasSession(sessionName); !exists {
		return fmt.Errorf("session %q not found", sessionName)
	}

	// Resolve nudge options once at startup: if the target agent reads Escape
	// as cancel (Claude Code does, gt-cyyg), skip the Escape keystroke during
	// delivery to avoid canceling in-flight generation. (GH#gt-wasn)
	//
	// TownRoot enables the cross-process nudge lock. Without it the poller's
	// send-keys can interleave with a concurrent `gt nudge` to the same
	// session, garbling the composer and failing submit verification — which
	// requeues the nudge and re-injects it (gt-tmlu).
	nudgeOpts := tmux.NudgeOpts{TownRoot: townRoot}
	hasPromptDetection := false
	if name, preset, ok := t.SessionAgentPreset(sessionName, townRoot); name != "" {
		if ok {
			hasPromptDetection = preset.ReadyPromptPrefix != ""
			nudgeOpts.SkipEscape = preset.EscapeCancelsRequest
		} else {
			nudgeOpts.SkipEscape = true // unidentified harness: fail safe (claude-9a8)
		}
	}

	// Set up signal handling for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	poller := &nudgePoller{
		sessionName: sessionName,
		hasSession:  t.HasSession,
		pending: func() int {
			n, _ := nudge.Pending(townRoot, sessionName)
			return n
		},
		waitForIdle: func() error { return t.WaitForIdle(sessionName, idleTimeout) },
		drain:       func() ([]nudge.QueuedNudge, error) { return nudge.Drain(townRoot, sessionName) },
		inject: func(msg string) error {
			return t.NudgeSessionWithOpts(sessionName, msg, nudgeOpts)
		},
		requeue: func(drained []nudge.QueuedNudge) {
			requeueDrainedNudges(townRoot, sessionName, "nudge-poller", drained)
		},
		hasPromptDetection: hasPromptDetection,
		stderr:             os.Stderr,
	}
	return poller.run(sigCh, ticker.C)
}

// nudgePollerMaxProbeErrors bounds how many session probes in a row may fail
// before the poller gives up. At the default poll interval that is about a
// minute of a broken sandbox or server; past it the session is treated as gone
// so the process does not outlive its target forever.
const nudgePollerMaxProbeErrors = 6

// nudgePoller holds the per-tick steps of the poll loop as funcs so tests can
// drive the loop without a tmux server.
type nudgePoller struct {
	sessionName        string
	hasSession         func(string) (bool, error)
	pending            func() int
	waitForIdle        func() error
	drain              func() ([]nudge.QueuedNudge, error)
	inject             func(string) error
	requeue            func([]nudge.QueuedNudge)
	hasPromptDetection bool
	stderr             io.Writer

	probeErrors int // consecutive failed session probes
}

// run polls on each tick until the session is gone or a signal arrives.
func (p *nudgePoller) run(sigCh <-chan os.Signal, ticks <-chan time.Time) error {
	for {
		select {
		case <-sigCh:
			return nil // graceful shutdown
		case <-ticks:
			if !p.tick() {
				return nil
			}
		}
	}
}

// tick runs one poll and reports whether the poller should keep running.
func (p *nudgePoller) tick() bool {
	if !p.sessionAlive() {
		return false // session gone, exit
	}

	// Check if there are queued nudges.
	if p.pending() == 0 {
		return true
	}

	// For runtimes with prompt detection, defer delivery until the session
	// is actually idle. Runtimes without prompt detection preserve the old
	// best-effort behavior and drain on the poll interval.
	if shouldSkipDrainUntilIdle(p.hasPromptDetection, p.waitForIdle()) {
		return true
	}

	// Drain and inject.
	drained, err := p.drain()
	if err != nil {
		fmt.Fprintf(p.stderr, "nudge-poller: drain error for %s: %v\n", p.sessionName, err)
		return true
	}
	if len(drained) == 0 {
		return true // someone else drained it
	}

	if err := p.inject(nudge.FormatForInjection(drained)); err != nil {
		fmt.Fprintf(p.stderr, "nudge-poller: injection error for %s: %v\n", p.sessionName, err)
		p.requeue(drained)
	}
	return true
}

func shouldSkipDrainUntilIdle(hasPromptDetection bool, waitErr error) bool {
	return hasPromptDetection && waitErr != nil
}

// sessionAlive reports whether the poller should keep running. A confirmed
// absence ends it. A probe error is transient (a failed sandbox exec, a busy
// server), so it is logged and the next tick probes again instead of dropping
// the queue; nudgePollerMaxProbeErrors failures in a row end the poller too.
func (p *nudgePoller) sessionAlive() bool {
	exists, err := p.hasSession(p.sessionName)
	if err != nil {
		p.probeErrors++
		fmt.Fprintf(p.stderr, "nudge-poller: session probe error for %s (%d/%d): %v\n",
			p.sessionName, p.probeErrors, nudgePollerMaxProbeErrors, err)
		return p.probeErrors < nudgePollerMaxProbeErrors
	}
	p.probeErrors = 0
	return exists
}

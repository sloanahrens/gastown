package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/nudge/deliver"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	nudgeMessageFlag  string
	nudgeForceFlag    bool
	nudgeStdinFlag    bool
	nudgeIfFreshFlag  bool
	nudgeModeFlag     string
	nudgePriorityFlag string
)

// Nudge delivery modes.
const (
	NudgeModeImmediate = deliver.ModeImmediate
	NudgeModeQueue     = deliver.ModeQueue
	NudgeModeWaitIdle  = deliver.ModeWaitIdle
)

func init() {
	rootCmd.AddCommand(nudgeCmd)
	nudgeCmd.Flags().StringVarP(&nudgeMessageFlag, "message", "m", "", "Message to send")
	nudgeCmd.Flags().BoolVarP(&nudgeForceFlag, "force", "f", false, "Send even if target has DND enabled, or --mode=immediate target is busy")
	nudgeCmd.Flags().BoolVar(&nudgeStdinFlag, "stdin", false, "Read message from stdin (avoids shell quoting issues)")
	nudgeCmd.Flags().BoolVar(&nudgeIfFreshFlag, "if-fresh", false, "Only send if caller's tmux session is <60s old (suppresses compaction nudges)")
	nudgeCmd.Flags().StringVar(&nudgeModeFlag, "mode", NudgeModeWaitIdle, "Delivery mode: wait-idle (default), queue, or immediate")
	nudgeCmd.Flags().StringVar(&nudgePriorityFlag, "priority", nudge.PriorityNormal, "Queue priority: normal (default) or urgent")
}

var nudgeCmd = &cobra.Command{
	Use:     "nudge <target> [message]",
	GroupID: GroupComm,
	Short:   "Send a synchronous message to any Gas Town worker",
	Long: `Universal messaging API for Gas Town worker-to-worker communication.

Delivers a message to any worker's Claude Code session: polecats, crew,
or the mayor.

Delivery modes (--mode):
  wait-idle  Wait for agent to become idle (prompt visible), then deliver
             directly. Falls back to queue on timeout. If both idle-wait and
             queue fail, falls back to immediate delivery as a last resort.
             This is the default — it avoids interrupting active tool calls.
  queue      Write to a file queue; agent picks up via hook at next turn
             boundary. Zero interruption. Use for non-urgent coordination.
  immediate  Send directly via tmux send-keys. Refuses to interrupt a busy
             target — falls back to wait-idle/queue delivery instead — unless
             --force is given. Use only when you need to break through
             (e.g., stuck agent, emergency). After delivery, immediate mode
             watches the target for a few seconds: if it started no turn (or
             the pane could not be read), it prints a warning to stderr —
             the exit code stays 0, because delivery itself succeeded.

Queue and wait-idle modes require a drain mechanism. Claude agents drain
via UserPromptSubmit hook; other agents use a background nudge-poller
that periodically drains and injects via tmux. If neither is available,
use --mode=immediate.

This is the ONLY way to send messages to Claude sessions.
Do not use raw tmux send-keys elsewhere.

Role shortcuts (expand to session names):
  mayor     Maps to gt-mayor

Channel syntax:
  channel:<name>  Nudges all members of a named channel defined in
                  <town-root>/config/messaging.json under "nudge_channels".
                  Patterns like "gastown/polecats/*" are expanded.

DND (Do Not Disturb):
  If the target has DND enabled, the nudge is skipped.
  Use --force to override DND and send anyway.

Examples:
  gt nudge greenplace/furiosa "Check your mail and start working"
  gt nudge greenplace/alpha -m "What's your status?"
  gt nudge mayor "Status update requested"
  gt nudge channel:workers "New priority work available"

  # Use --stdin for messages with special characters or formatting:
  gt nudge gastown/alpha --stdin <<'EOF'
  Status update:
  - Task 1: complete
  - Task 2: in progress
  EOF`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runNudge,
}

// ifFreshMaxAge is the maximum session age for --if-fresh to allow a nudge.
// Sessions older than this are considered compaction/clear restarts, not new sessions.
const ifFreshMaxAge = 60 * time.Second

// waitIdleTimeout is how long --mode=wait-idle will poll before falling back to queue.
// This is a var (not const) so tests can override it to avoid 15s waits.
var waitIdleTimeout = deliver.WaitIdleTimeout

// idleWatcherTimeout is how long the background idle watcher polls after
// queuing a nudge. If the agent becomes idle within this window, the watcher
// drains the queue and delivers directly. This covers the gap where an agent
// finishes work after WaitForIdle's timeout but before anyone sends new input
// (so UserPromptSubmit never fires and the queue never drains).
// Var so tests can override.
var idleWatcherTimeout = deliver.WatchTimeout

// idleWatcherPollInterval is how often the background watcher checks for idle.
// Var so tests can override.
var idleWatcherPollInterval = deliver.PollInterval

// newNudgeDelivery is the delivery the gt nudge flags and timings describe.
func newNudgeDelivery(t *tmux.Tmux, townRoot string) *deliver.Delivery {
	d := deliver.New(t, townRoot)
	d.Mode, d.Priority, d.Force = nudgeModeFlag, nudgePriorityFlag, nudgeForceFlag
	d.WaitIdleTimeout, d.WatchTimeout, d.PollInterval = waitIdleTimeout, idleWatcherTimeout, idleWatcherPollInterval
	d.ProbeWindow = immediateTurnProbeWindow
	d.Stderr = os.Stderr
	return d
}

// immediateTurnProbeWindow is how long immediate mode watches the pane for a
// reaction before reporting that the target did not start a turn. Var so tests
// can shorten it.
var immediateTurnProbeWindow = deliver.ProbeWindow

// consumptionWarning is deliver.ConsumptionWarning over t's consumption probe
// and immediateTurnProbeWindow.
func consumptionWarning(t *tmux.Tmux, sessionName, mode string) string {
	return deliver.ConsumptionWarning(t.WaitForInputConsumed, immediateTurnProbeWindow, sessionName, mode)
}

// immediateConsumptionWarning is consumptionWarning labeled for immediate mode.
func immediateConsumptionWarning(t *tmux.Tmux, sessionName string) string {
	return consumptionWarning(t, sessionName, NudgeModeImmediate)
}

// watchAndDeliver runs the post-queue idle watcher (deliver.Delivery.Watch)
// over the --priority flag and the idle-watcher timings.
func watchAndDeliver(t *tmux.Tmux, townRoot, sessionName string) {
	newNudgeDelivery(t, townRoot).Watch(context.Background(), sessionName)
}

func requeueDrainedNudges(townRoot, sessionName, source string, drained []nudge.QueuedNudge) {
	deliver.Requeue(os.Stderr, townRoot, sessionName, source, drained)
}

// validNudgeModes is the set of allowed --mode values.
var validNudgeModes = map[string]bool{
	NudgeModeImmediate: true,
	NudgeModeQueue:     true,
	NudgeModeWaitIdle:  true,
}

// validNudgePriorities is the set of allowed --priority values.
var validNudgePriorities = map[string]bool{
	nudge.PriorityNormal: true,
	nudge.PriorityUrgent: true,
}

// validateNudgeFlags rejects an unknown --mode or --priority.
func validateNudgeFlags(mode, priority string) error {
	if !validNudgeModes[mode] {
		return fmt.Errorf("invalid --mode %q: must be one of immediate, queue, wait-idle", mode)
	}
	if !validNudgePriorities[priority] {
		return fmt.Errorf("invalid --priority %q: must be one of normal, urgent", priority)
	}
	return nil
}

// nudgeTargetAndMessage reads the nudge target from args and the message
// from -m, --stdin (through readStdin) or the second argument.
func nudgeTargetAndMessage(messageFlag string, stdin bool, readStdin func() ([]byte, error), args []string) (target, message string, err error) {
	// Normalize trailing slash: the mail system uses "mayor/" as the
	// canonical address, but nudge role shortcuts expect bare names.
	// Without this, "mayor/" falls through to parseAddress which rejects
	// the empty second component, silently dropping the nudge.
	target = strings.TrimSuffix(args[0], "/")

	// Handle --stdin: read message from stdin (avoids shell quoting issues)
	if stdin {
		if messageFlag != "" {
			return "", "", fmt.Errorf("cannot use --stdin with --message/-m")
		}
		data, err := readStdin()
		if err != nil {
			return "", "", fmt.Errorf("reading stdin: %w", err)
		}
		messageFlag = strings.TrimRight(string(data), "\n")
	}

	// Get message from -m flag or positional arg
	switch {
	case messageFlag != "":
		message = messageFlag
	case len(args) >= 2:
		message = args[1]
	default:
		return "", "", fmt.Errorf("message required: use -m flag or provide as second argument")
	}
	return target, message, nil
}

func runNudge(cmd *cobra.Command, args []string) (retErr error) {
	// Validate --mode and --priority before doing anything else.
	if err := validateNudgeFlags(nudgeModeFlag, nudgePriorityFlag); err != nil {
		return err
	}

	// --if-fresh: skip nudge if the caller's tmux session is older than 60s.
	// This prevents compaction/clear SessionStart hooks from spamming the target.
	if nudgeIfFreshFlag {
		sessionName := tmux.CurrentSessionName()
		if sessionName != "" {
			t := tmux.NewTmux()
			created, err := t.GetSessionCreatedUnix(sessionName)
			if err == nil && created > 0 {
				age := time.Since(time.Unix(created, 0))
				if age > ifFreshMaxAge {
					// Session is old — this is a compaction/clear, not a new session
					return nil
				}
			}
		}
	}

	target, message, err := nudgeTargetAndMessage(nudgeMessageFlag, nudgeStdinFlag, func() ([]byte, error) { return io.ReadAll(os.Stdin) }, args)
	if err != nil {
		return err
	}

	townRoot, _ := workspace.FindFromCwd()
	if townRoot != "" {
		// Initialize tmux socket and prefix registry so NewTmux() connects
		// to the correct town socket. Without this, nudge from non-agent
		// contexts (e.g., crew workspaces without GT_TOWN_SOCKET) falls
		// through to the sentinel socket and fails to find sessions.
		_ = session.InitRegistry(townRoot)
	}
	cwd, _ := os.Getwd()
	sender := deliver.Sender(cwd, townRoot, os.Getenv)

	skipped := false
	town := &deliver.Town{
		Delivery: newNudgeDelivery(tmux.NewTmux(), townRoot),
		Registry: townRegistry(),
		Skipped: func(_, level string) {
			skipped = true
			fmt.Printf("%s Target has DND enabled (%s) - nudge skipped\n", style.Dim.Render("○"), level)
			fmt.Printf("  Use %s to override\n", style.Bold.Render("--force"))
		},
	}

	if channel, ok := strings.CutPrefix(target, "channel:"); ok {
		return runNudgeChannel(town, channel, message, sender)
	}
	if err := town.Nudge(context.Background(), target, message, sender); err != nil {
		return err
	}
	if !skipped {
		fmt.Printf("%s Nudged %s (%s)\n", style.Bold.Render("✓"), target, nudgeModeFlag)
	}
	return nil
}

// runNudgeChannel nudges all running members of a named channel through
// town, printing each member's outcome and a summary.
func runNudgeChannel(town *deliver.Town, channel, message, sender string) error {
	town.Skipped = nil
	town.Member = func(m deliver.ChannelMember) {
		switch {
		case m.DND != "":
			fmt.Printf("  %s %s (DND: %s)\n", style.Dim.Render("○"), m.Session, m.DND)
		case m.Err != nil:
			fmt.Printf("  %s %s\n", style.ErrorPrefix, m.Session)
		default:
			fmt.Printf("  %s %s\n", style.SuccessPrefix, m.Session)
		}
	}

	fmt.Printf("Nudging channel %q (mode=%s)...\n\n", channel, nudgeModeFlag)
	members, err := town.NudgeChannel(context.Background(), channel, message, sender)
	if members == nil {
		if err != nil {
			return err
		}
		fmt.Printf("%s No sessions match channel %q patterns\n", style.WarningPrefix, channel)
		return nil
	}
	fmt.Println()

	var succeeded, skipped int
	var failures []string
	for _, m := range members {
		switch {
		case m.DND != "":
			skipped++
		case m.Err != nil:
			failures = append(failures, fmt.Sprintf("%s: %v", m.Session, m.Err))
		default:
			succeeded++
		}
	}
	summary := fmt.Sprintf("Channel nudge complete: %d target(s) nudged", succeeded)
	if len(failures) > 0 {
		summary = fmt.Sprintf("Channel nudge complete: %d succeeded, %d failed", succeeded, len(failures))
	}
	if skipped > 0 {
		summary += fmt.Sprintf(", %d skipped (DND)", skipped)
	}
	if err != nil {
		fmt.Printf("%s %s\n", style.WarningPrefix, summary)
		for _, f := range failures {
			fmt.Printf("  %s\n", style.Dim.Render(f))
		}
		return err
	}
	fmt.Printf("%s %s\n", style.SuccessPrefix, summary)
	return nil
}

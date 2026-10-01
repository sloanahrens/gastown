package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/nudge"
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
	// NudgeModeImmediate sends directly via tmux send-keys (current behavior).
	// This interrupts in-flight work but guarantees immediate delivery.
	NudgeModeImmediate = "immediate"
	// NudgeModeQueue writes to a file queue; agent picks up via hook at next
	// turn boundary. Zero interruption but delivery depends on agent turn frequency.
	NudgeModeQueue = "queue"
	// NudgeModeWaitIdle waits for the agent to become idle (prompt visible),
	// then delivers directly. Falls back to queue on timeout. Best of both worlds.
	NudgeModeWaitIdle = "wait-idle"
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
var waitIdleTimeout = 15 * time.Second

// idleWatcherTimeout is how long the background idle watcher polls after
// queuing a nudge. If the agent becomes idle within this window, the watcher
// drains the queue and delivers directly. This covers the gap where an agent
// finishes work after WaitForIdle's timeout but before anyone sends new input
// (so UserPromptSubmit never fires and the queue never drains).
// Var so tests can override.
var idleWatcherTimeout = 60 * time.Second

// idleWatcherPollInterval is how often the background watcher checks for idle.
// Var so tests can override.
var idleWatcherPollInterval = 1 * time.Second

// deliverNudge routes a nudge to sessionName by the --mode, --priority and
// --force flags (nudgeDelivery.deliver).
func deliverNudge(t *tmux.Tmux, sessionName, message, sender string) error {
	// Test hook: when GT_TEST_NUDGE_LOG is set, log the nudge instead of
	// delivering through real tmux/queue transport. Prevents test-suite
	// runs from delivering "test" messages to live agents (mayor reported
	// recurring synthetic nudges traced to nudge_test.go invocations).
	// Mirrors the pattern in sling_helpers.go's nudgeWitness.
	if logPath := os.Getenv("GT_TEST_NUDGE_LOG"); logPath != "" {
		entry := fmt.Sprintf("nudge:%s:%s:%s\n", sessionName, sender, message)
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			_, _ = f.WriteString(entry)
			_ = f.Close()
		}
		return nil
	}

	townRoot, _ := workspace.FindFromCwd()
	return newNudgeDelivery(t, townRoot, os.Stderr).deliver(sessionName, message, sender)
}

// immediateTurnProbeWindow is how long immediate mode watches the pane for a
// reaction before reporting that the target did not start a turn. Var so tests
// can shorten it.
var immediateTurnProbeWindow = 3 * time.Second

// consumptionWarning watches a target for a reaction to a nudge that mode has
// just delivered directly (immediate, or wait-idle's own direct delivery once
// the target went idle), and returns a warning line when the target took the
// input without acting on it — or when consumption could not be established
// (an unreadable pane, or a frozen pane with nothing to date it by, both read
// UNKNOWN and name a re-probe instead of claiming a wedge, gt-7xnv). It
// returns "" only on a positive, unambiguous verdict.
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
func consumptionWarning(t *tmux.Tmux, sessionName, mode string) string {
	return consumptionWarningFor(t.WaitForInputConsumed, immediateTurnProbeWindow, sessionName, mode)
}

// consumptionWarningFor is consumptionWarning over a given probe and window:
// the message for the verdict the probe returns.
func consumptionWarningFor(probe func(sessionName string, window time.Duration) (tmux.InputConsumption, error), window time.Duration, sessionName, mode string) string {
	verdict, err := probe(sessionName, window)
	if err != nil {
		// Fail closed (gt-7xnv): an unobservable pane says nothing about the
		// nudge, so it must not read as one that was consumed — but it must not
		// claim a wedge either, so the word here is UNKNOWN, not "started no
		// turn". Re-probe before acting on it.
		return fmt.Sprintf(
			"%s: %s took the nudge but consumption is UNKNOWN — the probe could not read the pane (%v). "+
				"Re-check with 'gt session health %s' before acting.\n",
			mode, sessionName, err, sessionName)
	}
	if verdict == tmux.InputConsumptionUndated {
		// The pane was frozen and holding input but has no content above the
		// input box to date it by (gt-7xnv): the nudge may be the input, or
		// any older text. Not a strand claim — re-probe on a longer window.
		return fmt.Sprintf(
			"%s: %s still holds input and the pane was frozen for %s, but it has nothing above "+
				"the input box to date it by — consumption is UNKNOWN (UNDATED), not a strand. "+
				"Re-check with 'gt session health %s' before acting.\n",
			mode, sessionName, window, sessionName)
	}
	if verdict != tmux.InputConsumptionNotConsumed {
		return ""
	}
	return fmt.Sprintf(
		"%s: %s accepted the nudge but started no turn within %s — its input is "+
			"still stranded in the composer/queue, which is how a wedged session presents "+
			"(gt-eigw). Inspect it with 'gt session health %s'; if it stays stuck, restart "+
			"that session.\n",
		mode, sessionName, window, sessionName)
}

// immediateConsumptionWarning is consumptionWarning labeled for immediate mode.
func immediateConsumptionWarning(t *tmux.Tmux, sessionName string) string {
	return consumptionWarning(t, sessionName, NudgeModeImmediate)
}

// watchAndDeliver runs the post-queue idle watcher (nudgeDelivery.watch) over
// the --priority flag and the idle-watcher timings.
func watchAndDeliver(t *tmux.Tmux, townRoot, sessionName string) {
	newNudgeDelivery(t, townRoot, os.Stderr).watch(sessionName)
}

func requeueDrainedNudges(townRoot, sessionName, source string, drained []nudge.QueuedNudge) {
	if err := nudge.Requeue(townRoot, sessionName, drained); err != nil {
		fmt.Fprintf(os.Stderr, "%s: requeue for %s failed: %v\n", source, sessionName, err)
	}
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

	// Identify sender for message prefix (needed before channel check)
	sender := nudgeSender(GetRole())

	// Handle channel syntax: channel:<name>
	if strings.HasPrefix(target, "channel:") {
		channelName := strings.TrimPrefix(target, "channel:")
		return runNudgeChannel(channelName, message, sender)
	}

	// Check DND status for target (unless force flag or channel target)
	townRoot, _ := workspace.FindFromCwd()
	if townRoot != "" {
		// Initialize tmux socket and prefix registry so NewTmux() connects
		// to the correct town socket. Without this, nudge from non-agent
		// contexts (e.g., crew workspaces without GT_TOWN_SOCKET) falls
		// through to the sentinel socket and fails to find sessions.
		_ = session.InitRegistry(townRoot)
	}
	if townRoot != "" && !nudgeForceFlag {
		shouldSend, level, _ := shouldNudgeTarget(townRegistry(), townRoot, target, nudgeForceFlag)
		if !shouldSend {
			fmt.Printf("%s Target has DND enabled (%s) - nudge skipped\n", style.Dim.Render("○"), level)
			fmt.Printf("  Use %s to override\n", style.Bold.Render("--force"))
			return nil
		}
	}

	t := tmux.NewTmux()

	// Expand role shortcuts to session names
	// These shortcuts let users type "mayor" instead of "gt-mayor"
	if target == constants.RoleMayor {
		target = session.MayorSessionName()
	}

	if strings.HasPrefix(target, constants.RoleMayor+"/") || strings.HasPrefix(target, "deacon/") {
		return fmt.Errorf("invalid town target %q", target)
	}

	// Check if target is rig/polecat format or raw session name
	if strings.Contains(target, "/") {
		// Parse rig/polecat format
		rigName, polecatName, err := parseAddress(target)
		if err != nil {
			return err
		}

		var sessionName string

		// Check if this is a crew address (polecatName starts with "crew/")
		if strings.HasPrefix(polecatName, "crew/") {
			// Extract crew name and use crew session naming
			crewName := strings.TrimPrefix(polecatName, "crew/")
			sessionName = crewSessionName(townRegistry(), rigName, crewName)
		} else if strings.HasPrefix(polecatName, "polecats/") {
			// Explicit polecat address (e.g., "vastal/polecats/furiosa").
			// Bypasses crew-first resolution for short addresses.
			pcName := strings.TrimPrefix(polecatName, "polecats/")
			mgr, err := getSessionManager(rigName)
			if err != nil {
				return err
			}
			sessionName = mgr.SessionName(pcName)
		} else {
			// Short address (e.g., "gastown/holden") - could be crew or polecat.
			// Try crew first (matches mail system's addressToSessionIDs pattern),
			// then fall back to polecat.
			crewSession := crewSessionName(townRegistry(), rigName, polecatName)
			if exists, _ := t.HasSession(crewSession); exists {
				sessionName = crewSession
			} else {
				mgr, err := getSessionManager(rigName)
				if err != nil {
					return err
				}
				sessionName = mgr.SessionName(polecatName)
			}
		}

		// For queue/wait-idle modes, verify session exists before enqueuing.
		// Without this, queue mode silently succeeds for nonexistent sessions —
		// the file is written but never drained.
		if nudgeModeFlag != NudgeModeImmediate {
			exists, err := t.HasSession(sessionName)
			if err != nil {
				return fmt.Errorf("checking session: %w", err)
			}
			if !exists {
				return fmt.Errorf("session %q not found (cannot queue nudge for nonexistent session)", sessionName)
			}
		}

		// Send nudge using the configured delivery mode
		if err := deliverNudge(t, sessionName, message, sender); err != nil {
			return fmt.Errorf("nudging session: %w", err)
		}

		fmt.Printf("%s Nudged %s/%s (%s)\n", style.Bold.Render("✓"), rigName, polecatName, nudgeModeFlag)

		// Log nudge event
		if townRoot, err := workspace.FindFromCwd(); err == nil && townRoot != "" {
			_ = LogNudge(townRoot, target, message)
		}
		_ = events.LogFeed(events.TypeNudge, sender, events.NudgePayload(rigName, target, message))
	} else {
		// Raw session name (legacy)
		exists, err := t.HasSession(target)
		if err != nil {
			return fmt.Errorf("checking session: %w", err)
		}
		if !exists {
			return fmt.Errorf("session %q not found", target)
		}

		if err := deliverNudge(t, target, message, sender); err != nil {
			return fmt.Errorf("nudging session: %w", err)
		}

		fmt.Printf("✓ Nudged %s (%s)\n", target, nudgeModeFlag)

		// Log nudge event
		if townRoot, err := workspace.FindFromCwd(); err == nil && townRoot != "" {
			_ = LogNudge(townRoot, target, message)
		}
		_ = events.LogFeed(events.TypeNudge, sender, events.NudgePayload("", target, message))
	}

	return nil
}

// nudgeSender is the sender a nudge from the caller's role is attributed to:
// "unknown" when the role cannot be read.
func nudgeSender(roleInfo RoleInfo, err error) string {
	if err != nil {
		return "unknown"
	}
	switch roleInfo.Role {
	case RoleMayor:
		return constants.RoleMayor
	case RoleCrew:
		return fmt.Sprintf("%s/crew/%s", roleInfo.Rig, roleInfo.Polecat)
	case RolePolecat:
		return fmt.Sprintf("%s/%s", roleInfo.Rig, roleInfo.Polecat)
	default:
		return string(roleInfo.Role)
	}
}

// runNudgeChannel nudges all members of a named channel.
// Routes each target through deliverNudge so --mode is respected.
func runNudgeChannel(channelName, message, sender string) error {
	// Find town root
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("cannot find town root: %w", err)
	}

	// Load messaging config
	msgConfigPath := config.MessagingConfigPath(townRoot)
	msgConfig, err := config.LoadMessagingConfig(msgConfigPath)
	if err != nil {
		return fmt.Errorf("loading messaging config: %w", err)
	}

	// Look up channel
	patterns, ok := msgConfig.NudgeChannels[channelName]
	if !ok {
		return fmt.Errorf("nudge channel %q not found in messaging config", channelName)
	}

	if len(patterns) == 0 {
		return fmt.Errorf("nudge channel %q has no members", channelName)
	}

	// Get all running sessions for pattern matching
	reg := townRegistry()
	agents, err := getAgentSessions(reg, true)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	// Resolve patterns to session names
	var targets []string
	seenTargets := make(map[string]bool)

	for _, pattern := range patterns {
		resolved := resolveNudgePattern(pattern, agents)
		for _, sessionName := range resolved {
			if !seenTargets[sessionName] {
				seenTargets[sessionName] = true
				targets = append(targets, sessionName)
			}
		}
	}

	if len(targets) == 0 {
		fmt.Printf("%s No sessions match channel %q patterns\n", style.WarningPrefix, channelName)
		return nil
	}

	// Send nudges via deliverNudge (respects --mode flag)
	t := tmux.NewTmux()
	var succeeded, failed, skipped int
	var failures []string

	fmt.Printf("Nudging channel %q (%d target(s), mode=%s)...\n\n", channelName, len(targets), nudgeModeFlag)

	for i, sessionName := range targets {
		// Check DND status before nudging each target
		// Convert session name back to address format for DND lookup
		targetAddr := sessionNameToAddress(reg, sessionName)
		if targetAddr != "" {
			if shouldSend, level, _ := shouldNudgeTarget(reg, townRoot, targetAddr, false); !shouldSend {
				skipped++
				fmt.Printf("  %s %s (DND: %s)\n", style.Dim.Render("○"), sessionName, level)
				continue
			}
		}

		if err := deliverNudge(t, sessionName, message, sender); err != nil {
			failed++
			failures = append(failures, fmt.Sprintf("%s: %v", sessionName, err))
			fmt.Printf("  %s %s\n", style.ErrorPrefix, sessionName)
		} else {
			succeeded++
			fmt.Printf("  %s %s\n", style.SuccessPrefix, sessionName)
		}

		// Small delay between nudges
		if i < len(targets)-1 {
			clockwork.NewRealClock().Sleep(100 * time.Millisecond)
		}
	}

	fmt.Println()

	// Log nudge event
	_ = events.LogFeed(events.TypeNudge, sender, events.NudgePayload("", "channel:"+channelName, message))

	if failed > 0 {
		summary := fmt.Sprintf("Channel nudge complete: %d succeeded, %d failed", succeeded, failed)
		if skipped > 0 {
			summary += fmt.Sprintf(", %d skipped (DND)", skipped)
		}
		fmt.Printf("%s %s\n", style.WarningPrefix, summary)
		for _, f := range failures {
			fmt.Printf("  %s\n", style.Dim.Render(f))
		}
		return fmt.Errorf("%d nudge(s) failed", failed)
	}

	summary := fmt.Sprintf("Channel nudge complete: %d target(s) nudged", succeeded)
	if skipped > 0 {
		summary += fmt.Sprintf(", %d skipped (DND)", skipped)
	}
	fmt.Printf("%s %s\n", style.SuccessPrefix, summary)
	return nil
}

// resolveNudgePattern resolves a nudge channel pattern to session names.
// Patterns can be:
//   - Literal: "gastown/crew/max" → gt-crew-max
//   - Wildcard: "gastown/polecats/*" → all polecat sessions in gastown
//   - Role: "*/crew/*" → all crew sessions
//   - Special: "mayor" → hq-mayor
func resolveNudgePattern(pattern string, agents []*AgentSession) []string {
	var results []string

	// Handle special cases
	if pattern == constants.RoleMayor {
		return []string{session.MayorSessionName()}
	}

	// Parse pattern
	if !strings.Contains(pattern, "/") {
		// Unknown pattern format
		return nil
	}

	parts := strings.SplitN(pattern, "/", 2)
	rigPattern := parts[0]
	targetPattern := parts[1]

	for _, agent := range agents {
		// Match rig pattern
		if rigPattern != "*" && rigPattern != agent.Rig {
			continue
		}

		// Match target pattern
		if strings.HasPrefix(targetPattern, "polecats/") {
			// polecats/* or polecats/<name>
			if agent.Type != AgentPolecat {
				continue
			}
			suffix := strings.TrimPrefix(targetPattern, "polecats/")
			if suffix != "*" && suffix != agent.AgentName {
				continue
			}
		} else if strings.HasPrefix(targetPattern, "crew/") {
			// crew/* or crew/<name>
			if agent.Type != AgentCrew {
				continue
			}
			suffix := strings.TrimPrefix(targetPattern, "crew/")
			if suffix != "*" && suffix != agent.AgentName {
				continue
			}
		} else {
			// Assume it's a polecat name (legacy short format)
			if agent.Type != AgentPolecat || agent.AgentName != targetPattern {
				continue
			}
		}

		results = append(results, agent.Name)
	}

	return results
}

// shouldNudgeTarget checks if a nudge should be sent based on the target's notification level.
// Returns (shouldSend bool, level string, err error).
// If force is true, always returns true.
// If the agent bead cannot be found, returns true (fail-open for backward compatibility).
func shouldNudgeTarget(reg *session.PrefixRegistry, townRoot, targetAddress string, force bool) (bool, string, error) { //nolint:unparam // error return kept for future use
	if force {
		return true, "", nil
	}

	// Try to determine agent bead ID from address
	agentBeadID := addressToAgentBeadID(reg, targetAddress)
	if agentBeadID == "" {
		// Can't determine agent bead, allow the nudge
		return true, "", nil
	}

	bd := beads.New(townRoot)
	level, err := bd.GetAgentNotificationLevel(agentBeadID)
	if err != nil {
		// Agent bead might not exist, allow the nudge
		return true, "", nil
	}

	// Allow nudge if level is not muted
	return level != beads.NotifyMuted, level, nil
}

// sessionNameToAddress converts a tmux session name back to a mail address
// for DND lookup. Returns empty string if the format is unrecognized.
// Examples:
//   - "gt-gastown-crew-max" -> "gastown/crew/max"
//   - "gt-gastown-alpha" -> "gastown/alpha"
//   - "hq-mayor" -> "mayor"
func sessionNameToAddress(reg *session.PrefixRegistry, sessionName string) string {
	identity, err := session.ParseSessionNameWithRegistry(sessionName, reg)
	if err != nil {
		return ""
	}

	// Use short address format: rig/name (not rig/polecats/name)
	switch identity.Role {
	case session.RoleMayor:
		return constants.RoleMayor
	case session.RoleCrew:
		return fmt.Sprintf("%s/crew/%s", identity.Rig, identity.Name)
	case session.RolePolecat:
		return fmt.Sprintf("%s/%s", identity.Rig, identity.Name)
	default:
		return ""
	}
}

// addressToAgentBeadID converts a target address to an agent bead ID.
// Examples:
//   - "mayor" -> "hq-mayor"
//   - "gastown/alpha" -> "gt-alpha"
//
// Returns empty string if the address cannot be converted.
func addressToAgentBeadID(reg *session.PrefixRegistry, address string) string {
	// Handle special cases
	switch address {
	case constants.RoleMayor, constants.RoleMayor + "/":
		return session.MayorSessionName()
	}
	if strings.HasPrefix(address, constants.RoleMayor+"/") || strings.HasPrefix(address, "deacon/") {
		return ""
	}

	// Parse rig/role format
	if !strings.Contains(address, "/") {
		return ""
	}

	parts := strings.SplitN(address, "/", 2)
	if len(parts) != 2 {
		return ""
	}

	rig := parts[0]
	role := parts[1]

	if strings.HasPrefix(role, "crew/") {
		crewName := strings.TrimPrefix(role, "crew/")
		return session.CrewSessionName(reg.PrefixForRig(rig), crewName)
	}
	if strings.HasPrefix(role, "polecats/") {
		pcName := strings.TrimPrefix(role, "polecats/")
		return session.PolecatSessionName(reg.PrefixForRig(rig), pcName)
	}
	// Assume polecat
	return session.PolecatSessionName(reg.PrefixForRig(rig), role)
}

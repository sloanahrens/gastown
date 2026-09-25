package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	awaitSignalTimeout     string
	awaitSignalBackoffBase string
	awaitSignalBackoffMult int
	awaitSignalBackoffMax  string
	awaitSignalQuiet       bool
	awaitSignalAgentBead   string
	awaitSignalRig         string
)

// awaitSignalRigAny is the --rig value that disables rig scoping, restoring the
// town-wide subscription await-signal had before gt-qwfp. Town-level agents use
// it: deacon/ is not a rig, so no rig context resolves for it.
const awaitSignalRigAny = "town"

var moleculeAwaitSignalCmd = &cobra.Command{
	Use:   "await-signal",
	Short: "Wait for activity feed signal with timeout",
	Long: `Wait for activity relevant to this rig on the events feed, with optional backoff.

This command is the primary wake mechanism for patrol agents. It tails
~/gt/.events.jsonl and returns when an event relevant to YOUR rig is appended
(slings, nudges, mail, spawns, and completions inside the rig).

RIG SCOPING (gt-qwfp):
The subscription is scoped to one rig. An event counts as a signal when its
actor is that rig or an agent inside it ("om", "om/witness"), or when the event
addresses the rig — mail/nudge "to"/"target" of "om/witness", a sling targeting
"om/polecats/jasper", a spawn with "rig":"om". Everything else is another rig's
business and is skipped, so an idle rig still reaches its backoff cap and runs
abbreviated patrols. Town-wide events that must wake a specific agent should be
sent as a nudge or mail to that agent, which is already matched.

The scope comes from --rig, then GT_RIG, then the registered rig containing the
current directory. With no rig context the subscription stays town-wide, so
town-level agents (mayor, deacon) behave as before. Pass --rig town to ask for
that explicitly.

If no relevant activity occurs within the timeout, the command returns with exit
code 0 but sets the AWAIT_SIGNAL_REASON environment variable to "timeout".

The timeout can be specified directly or via backoff configuration for
exponential wait patterns.

BACKOFF MODE:
When backoff parameters are provided, the effective timeout is calculated as:
  min(base * multiplier^idle_cycles, max)

The idle_cycles value is read from the agent bead's "idle" label, enabling
exponential backoff that persists across invocations. A timeout increments
idle:N; a signal resets it to idle:0 (the caller does not need to).

A single backoff wait never exceeds 9m, whatever --backoff-max says, so it fits
inside a 10-minute agent tool call.

EXIT CODES:
  0 - Signal received or timeout (check output for which)
  1 - Error opening events file

EXAMPLES:
  # Simple wait with 60s timeout (canonical form)
  gt mol step await-signal --timeout 60s

  # Short form (alias)
  gt mol await-signal --timeout 60s

  # Backoff mode with agent bead tracking:
  gt mol await-signal --agent-bead gt-gastown-witness \
    --backoff-base 30s --backoff-mult 2 --backoff-max 15m

  # Explicit rig scope (default: GT_RIG, else the rig containing cwd)
  gt mol await-signal --rig om --agent-bead om-witness --backoff-base 30s

  # Town-wide subscription (no rig scoping) for a town-level agent
  gt mol await-signal --rig town --agent-bead hq-deacon --backoff-base 30s

  # On timeout, the agent bead's idle:N label is auto-incremented
  # On signal, it is reset to idle:0 automatically

  # Quiet mode (no output, for scripting)
  gt mol await-signal --timeout 30s --quiet`,
	RunE: runMoleculeAwaitSignal,
}

// moleculeAwaitSignalShortcutCmd is a separate command instance that allows
// "gt mol await-signal" in addition to the canonical "gt mol step await-signal".
// A separate instance is required because cobra does not support a single
// command having two parents (AddCommand overwrites the parent pointer).
var moleculeAwaitSignalShortcutCmd = &cobra.Command{
	Use:   "await-signal",
	Short: "Wait for activity feed signal with timeout (alias: gt mol step await-signal)",
	Long:  moleculeAwaitSignalCmd.Long,
	RunE:  runMoleculeAwaitSignal,
}

// AwaitSignalResult is the result of an await-signal operation.
type AwaitSignalResult struct {
	Reason      string        `json:"reason"`                // "signal" or "timeout"
	Elapsed     time.Duration `json:"elapsed"`               // how long we waited
	Signal      string        `json:"signal,omitempty"`      // the line that woke us (if signal)
	IdleCycles  int           `json:"idle_cycles,omitempty"` // current idle cycle count (after update)
	EffortLevel string        `json:"effort_level"`          // "full" or "abbreviated"
	// Nudges holds any nudges queued for this session and drained while
	// awaiting the signal. await-signal runs every patrol cycle regardless of
	// how long the previous gate took, so it is a step boundary a long-running
	// patrol turn actually passes through — unlike the UserPromptSubmit hook,
	// which never fires mid-turn (gt-saz7a).
	Nudges []nudge.QueuedNudge `json:"nudges,omitempty"`
}

func init() {
	moleculeAwaitSignalCmd.Flags().StringVar(&awaitSignalTimeout, "timeout", "60s",
		"Maximum time to wait for signal (e.g., 30s, 5m)")
	moleculeAwaitSignalCmd.Flags().StringVar(&awaitSignalBackoffBase, "backoff-base", "",
		"Base interval for exponential backoff (e.g., 30s)")
	moleculeAwaitSignalCmd.Flags().IntVar(&awaitSignalBackoffMult, "backoff-mult", 2,
		"Multiplier for exponential backoff (default: 2)")
	moleculeAwaitSignalCmd.Flags().StringVar(&awaitSignalBackoffMax, "backoff-max", "",
		"Maximum interval cap for backoff (e.g., 5m; never above 9m)")
	moleculeAwaitSignalCmd.Flags().StringVar(&awaitSignalAgentBead, "agent-bead", "",
		"Agent bead ID for tracking idle cycles (reads/writes idle:N label)")
	moleculeAwaitSignalCmd.Flags().StringVar(&awaitSignalRig, "rig", "",
		"Rig scope for signals (default: GT_RIG or rig containing cwd; 'town' accepts any rig)")
	moleculeAwaitSignalCmd.Flags().BoolVar(&awaitSignalQuiet, "quiet", false,
		"Suppress output (for scripting)")
	moleculeAwaitSignalCmd.Flags().BoolVar(&moleculeJSON, "json", false,
		"Output as JSON")

	moleculeStepCmd.AddCommand(moleculeAwaitSignalCmd)

	// Register shortcut flags on the shortcut command (shares the same global vars)
	moleculeAwaitSignalShortcutCmd.Flags().StringVar(&awaitSignalTimeout, "timeout", "60s",
		"Maximum time to wait for signal (e.g., 30s, 5m)")
	moleculeAwaitSignalShortcutCmd.Flags().StringVar(&awaitSignalBackoffBase, "backoff-base", "",
		"Base interval for exponential backoff (e.g., 30s)")
	moleculeAwaitSignalShortcutCmd.Flags().IntVar(&awaitSignalBackoffMult, "backoff-mult", 2,
		"Multiplier for exponential backoff (default: 2)")
	moleculeAwaitSignalShortcutCmd.Flags().StringVar(&awaitSignalBackoffMax, "backoff-max", "",
		"Maximum interval cap for backoff (e.g., 5m; never above 9m)")
	moleculeAwaitSignalShortcutCmd.Flags().StringVar(&awaitSignalAgentBead, "agent-bead", "",
		"Agent bead ID for tracking idle cycles (reads/writes idle:N label)")
	moleculeAwaitSignalShortcutCmd.Flags().StringVar(&awaitSignalRig, "rig", "",
		"Rig scope for signals (default: GT_RIG or rig containing cwd; 'town' accepts any rig)")
	moleculeAwaitSignalShortcutCmd.Flags().BoolVar(&awaitSignalQuiet, "quiet", false,
		"Suppress output (for scripting)")
	moleculeAwaitSignalShortcutCmd.Flags().BoolVar(&moleculeJSON, "json", false,
		"Output as JSON")

	// alias: gt mol await-signal (in addition to gt mol step await-signal)
	moleculeCmd.AddCommand(moleculeAwaitSignalShortcutCmd)
}

func runMoleculeAwaitSignal(cmd *cobra.Command, args []string) error {
	// Find beads directory (rig-local for bead operations)
	beadsDir, err := resolveAgentTrackingBeadsDir()
	if err != nil {
		return fmt.Errorf("not in a beads workspace: %w", err)
	}

	// Find town root for events file (events are always at <townRoot>/.events.jsonl)
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Read current idle cycles and backoff window from agent bead (if specified)
	var idleCycles int
	var backoffUntil time.Time // zero value means no active window
	if awaitSignalAgentBead != "" {
		labels, err := getAgentLabels(awaitSignalAgentBead, beadsDir)
		if err != nil {
			// Agent bead might not exist yet - that's OK, start at 0
			if !awaitSignalQuiet {
				fmt.Printf("%s Could not read agent bead (starting at idle=0): %v\n",
					style.Dim.Render("⚠"), err)
			}
		} else {
			if idleStr, ok := labels["idle"]; ok {
				if n, err := parseIntSimple(idleStr); err == nil {
					idleCycles = n
				}
			}
			if untilStr, ok := labels["backoff-until"]; ok {
				if ts, err := parseIntSimple(untilStr); err == nil && ts > 0 {
					backoffUntil = time.Unix(int64(ts), 0)
				}
			}
		}
	}

	// Calculate full timeout from backoff formula (uses idle cycles)
	fullTimeout, err := calculateEffectiveTimeout(idleCycles)
	if err != nil {
		return fmt.Errorf("invalid timeout configuration: %w", err)
	}

	// Determine effective timeout: resume from persisted window or start fresh.
	// This makes backoff resilient to interrupts (e.g., nudges that kill the
	// running await-signal). If the process is interrupted and relaunched within
	// the same backoff window, it sleeps only for the remaining time.
	timeout := fullTimeout
	resumed := false
	now := time.Now()
	if awaitSignalAgentBead != "" && !backoffUntil.IsZero() && backoffUntil.After(now) {
		remaining := backoffUntil.Sub(now)
		// Sanity: remaining should not exceed the calculated full timeout.
		// If idle:N was reset externally, the stored window may be stale.
		if remaining <= fullTimeout {
			timeout = remaining
			resumed = true
		}
	}

	// Persist the backoff window end time so interrupted invocations can resume.
	if awaitSignalAgentBead != "" && !resumed {
		windowEnd := now.Add(timeout)
		if err := setAgentBackoffUntil(awaitSignalAgentBead, beadsDir, windowEnd); err != nil {
			if !awaitSignalQuiet {
				fmt.Printf("%s Failed to persist backoff window: %v\n",
					style.Dim.Render("⚠"), err)
			}
		}
	}

	// Scope the subscription to this rig (gt-qwfp). Without it the town-wide
	// feed wakes an idle rig's witness on every other rig's activity, so the
	// idle backoff never grows and full patrols run back-to-back.
	rigScope := resolveEventRig(townRoot, awaitSignalRig)
	if rigScope == awaitSignalRigAny {
		rigScope = ""
	}

	if !awaitSignalQuiet && !moleculeJSON {
		scope := "town-wide"
		if rigScope != "" {
			scope = "rig " + rigScope
		}
		if resumed {
			fmt.Printf("%s Resuming backoff (remaining: %v, idle: %d, %s)...\n",
				style.Dim.Render("⏳"), timeout.Round(time.Second), idleCycles, scope)
		} else if awaitSignalAgentBead != "" {
			fmt.Printf("%s Awaiting signal (timeout: %v, idle: %d, %s)...\n",
				style.Dim.Render("⏳"), timeout, idleCycles, scope)
		} else {
			fmt.Printf("%s Awaiting signal (timeout: %v, %s)...\n",
				style.Dim.Render("⏳"), timeout, scope)
		}
	}

	startTime := time.Now()

	// Tail events file for new activity
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	result, err := waitForActivitySignal(ctx, townRoot, rigScope)
	if err != nil {
		return fmt.Errorf("feed subscription failed: %w", err)
	}

	result.Elapsed = time.Since(startTime)

	// On timeout, increment idle cycles and clear backoff window
	if result.Reason == "timeout" && awaitSignalAgentBead != "" {
		newIdleCycles := idleCycles + 1
		if err := setAgentIdleCycles(awaitSignalAgentBead, beadsDir, newIdleCycles); err != nil {
			if !awaitSignalQuiet {
				fmt.Printf("%s Failed to update agent bead idle count: %v\n",
					style.Dim.Render("⚠"), err)
			}
		} else {
			result.IdleCycles = newIdleCycles
		}
		// Update last_activity so watchers know agent is still alive
		if err := updateAgentHeartbeat(awaitSignalAgentBead, beadsDir); err != nil {
			if !awaitSignalQuiet {
				fmt.Printf("%s Failed to update agent heartbeat: %v\n",
					style.Dim.Render("⚠"), err)
			}
		}
		// Clear the backoff window — timeout completed normally
		_ = clearAgentBackoffUntil(awaitSignalAgentBead, beadsDir)
	} else if result.Reason == "signal" && awaitSignalAgentBead != "" {
		// On signal, update last_activity to prove agent is alive
		if err := updateAgentHeartbeat(awaitSignalAgentBead, beadsDir); err != nil {
			if !awaitSignalQuiet {
				fmt.Printf("%s Failed to update agent heartbeat: %v\n",
					style.Dim.Render("⚠"), err)
			}
		}
		// Woken by real activity: reset the idle counter here so the next
		// wait starts at the base interval. This used to be left to the
		// agent, which skipped it (0 of 20 in one deacon session), so every
		// wait sat at the backoff cap (claude-9jq).
		result.IdleCycles = idleCycles
		if idleCycles > 0 {
			if err := setAgentIdleCycles(awaitSignalAgentBead, beadsDir, 0); err != nil {
				if !awaitSignalQuiet {
					fmt.Printf("%s Failed to reset agent bead idle count: %v\n",
						style.Dim.Render("⚠"), err)
				}
			} else {
				result.IdleCycles = 0
			}
		}
		// Clear the backoff window — woken by real activity
		_ = clearAgentBackoffUntil(awaitSignalAgentBead, beadsDir)
	}

	// Set effort level based on idle cycles.
	// On signal (activity detected) or first cycle (idle=0): full effort.
	// On timeout with idle > 0: abbreviated effort (skip optional patrol steps).
	if result.Reason == "signal" || result.IdleCycles == 0 {
		result.EffortLevel = "full"
	} else {
		result.EffortLevel = "abbreviated"
	}

	// Drain nudges queued for this session (gt-saz7a). Included in the JSON
	// result so a --json caller doesn't lose them; printed as a
	// system-reminder block below for the normal (human-readable) path.
	result.Nudges = drainSessionNudges(townRoot)

	// Output result
	if moleculeJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	if !awaitSignalQuiet {
		switch result.Reason {
		case "signal":
			fmt.Printf("%s Signal received after %v\n",
				style.Bold.Render("✓"), result.Elapsed.Round(time.Millisecond))
			if result.Signal != "" {
				// Truncate long signals
				sig := result.Signal
				if len(sig) > 80 {
					sig = sig[:77] + "..."
				}
				fmt.Printf("  %s\n", style.Dim.Render(sig))
			}
		case "timeout":
			if awaitSignalAgentBead != "" {
				fmt.Printf("%s Timeout after %v (idle cycle: %d)\n",
					style.Dim.Render("⏱"), result.Elapsed.Round(time.Millisecond), result.IdleCycles)
			} else {
				fmt.Printf("%s Timeout after %v (no activity)\n",
					style.Dim.Render("⏱"), result.Elapsed.Round(time.Millisecond))
			}
		}

		// Output effort recommendation for the next patrol cycle.
		if result.EffortLevel == "abbreviated" {
			fmt.Printf("\n%s Run ABBREVIATED patrol: quick checks only, skip optional steps.\n",
				style.Bold.Render("EFFORT: reduced"))
		} else {
			fmt.Printf("\n%s Run full patrol.\n",
				style.Bold.Render("EFFORT: full"))
		}
	}

	// Surface drained nudges unconditionally — even under --quiet — so a
	// long-running patrol turn sees them as soon as it reaches this step
	// boundary rather than losing them to a suppressed progress message.
	if len(result.Nudges) > 0 {
		fmt.Print(nudge.FormatForInjection(result.Nudges))
	}

	return nil
}

// awaitSignalMaxWait caps a single backoff-mode wait, whatever --backoff-max
// says. Patrol agents run await-signal as one Bash tool call, and Claude Code
// caps a tool call at 10 minutes: a longer wait is moved to the background and
// the agent falls back to sleep-polling it. The deacon formula's 15m cap did
// exactly that on every idle cycle (claude-9jq). Nine minutes leaves a minute
// for the bd reads and writes around the wait.
const awaitSignalMaxWait = 9 * time.Minute

// calculateEffectiveTimeout determines the timeout based on flags.
// If backoff parameters are provided, uses exponential backoff formula:
//
//	min(base * multiplier^idleCycles, max, awaitSignalMaxWait)
//
// Otherwise uses the simple --timeout value, which is not clamped: an explicit
// timeout is the caller's choice.
func calculateEffectiveTimeout(idleCycles int) (time.Duration, error) {
	timeout, err := backoffTimeout(idleCycles)
	if err != nil || awaitSignalBackoffBase == "" {
		return timeout, err
	}
	if timeout > awaitSignalMaxWait {
		return awaitSignalMaxWait, nil
	}
	return timeout, nil
}

// backoffTimeout is calculateEffectiveTimeout before the single-wait clamp.
func backoffTimeout(idleCycles int) (time.Duration, error) {
	// If backoff base is set, use backoff mode
	if awaitSignalBackoffBase != "" {
		base, err := time.ParseDuration(awaitSignalBackoffBase)
		if err != nil {
			return 0, fmt.Errorf("invalid backoff-base: %w", err)
		}

		// Apply exponential backoff: base * multiplier^idleCycles, capped at max.
		// Parse max first so we can cap early inside the loop and prevent
		// int64 overflow — time.Duration wraps negative around idle ~62+.
		var maxDur time.Duration
		if awaitSignalBackoffMax != "" {
			maxDur, err = time.ParseDuration(awaitSignalBackoffMax)
			if err != nil {
				return 0, fmt.Errorf("invalid backoff-max: %w", err)
			}
		}

		timeout := base
		for i := 0; i < idleCycles; i++ {
			// Cap early to prevent int64 overflow at high idle counts.
			if maxDur > 0 && timeout >= maxDur {
				return maxDur, nil
			}
			timeout *= time.Duration(awaitSignalBackoffMult)
		}
		if maxDur > 0 && timeout > maxDur {
			return maxDur, nil
		}

		return timeout, nil
	}

	// Simple timeout mode
	return time.ParseDuration(awaitSignalTimeout)
}

// waitForActivitySignal tails the events file for new activity relevant to rig.
// townRoot is the Gas Town workspace root; the events file is at
// <townRoot>/.events.jsonl. An empty rig accepts events from any rig. Returns
// immediately when a relevant event line is appended, or when context is
// canceled.
func waitForActivitySignal(ctx context.Context, townRoot, rig string) (*AwaitSignalResult, error) {
	return waitForEventsFile(ctx, filepath.Join(townRoot, events.EventsFile), rig)
}

// waitForEventsFile tails the events file for new lines relevant to rig.
// Lines belonging to other rigs are consumed and discarded so the wait
// continues; only a relevant line ends it. This replaces the former
// bd activity --follow subprocess approach.
//
// The tail follows the path, not the descriptor: the daemon's KRC pruner
// replaces the file (tmp + rename) on start and hourly, and a waiter still
// reading the old inode used to sleep through every event to its timeout
// (claude-9jq). events.Tail reopens the new file and resumes after the last
// line already seen, so retained history is not replayed.
func waitForEventsFile(ctx context.Context, eventsPath, rig string) (*AwaitSignalResult, error) {
	tail, err := events.OpenTail(eventsPath)
	if err != nil {
		return nil, fmt.Errorf("opening events file %s: %w", eventsPath, err)
	}
	defer func() { _ = tail.Close() }()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return &AwaitSignalResult{
				Reason: "timeout",
			}, nil
		case <-ticker.C:
			// Poll drains every complete line appended so far (partial lines
			// are held until their newline arrives). Reading one line per tick
			// would let a busy town outrun the reader, delaying a signal for
			// this rig indefinitely, since cross-rig lines are skipped.
			lines, err := tail.Poll()
			for _, line := range lines {
				if eventRelevantToRig(line, rig) {
					return &AwaitSignalResult{
						Reason: "signal",
						Signal: line,
					}, nil
				}
			}
			if err != nil {
				return nil, fmt.Errorf("reading events file: %w", err)
			}
		}
	}
}

// eventRelevantToRig reports whether a raw .events.jsonl line should wake a
// waiter scoped to rig. An empty rig accepts every line (town-wide scope).
//
// Relevance is deliberately two-sided: a rig's activity looks like events its
// own agents emit (actor "om/witness") AND events other agents aim at it (mail
// to "om/witness", a sling targeting "om/polecats/jasper", a spawn with
// "rig":"om"). Cross-rig events matching neither are what kept idle rigs at
// full effort, so they are skipped (gt-qwfp).
func eventRelevantToRig(line, rig string) bool {
	if rig == "" {
		return true
	}

	var ev events.Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		// Nothing to attribute to a rig. Skipping keeps a truncated or
		// non-JSON line from spending a full patrol.
		return false
	}

	if addressInRig(ev.Actor, rig) {
		return true
	}
	// Payload keys that name a destination: "target"/"to" for slings, nudges,
	// mail and escalations, "rig" for spawns and boots.
	for _, key := range []string{"target", "to", "rig"} {
		if addr, ok := ev.Payload[key].(string); ok && addressInRig(addr, rig) {
			return true
		}
	}
	return false
}

// addressInRig reports whether an actor or recipient address names rig or an
// agent inside it: "om" and "om/witness" belong to rig om, "mayor" does not.
// A trailing slash is tolerated because some emitters log "mayor/".
func addressInRig(addr, rig string) bool {
	if addr == "" || rig == "" {
		return false
	}
	addr = strings.TrimSuffix(addr, "/")
	return addr == rig || strings.HasPrefix(addr, rig+"/")
}

// parseIntSimple parses a string to int without using strconv.
func parseIntSimple(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty string")
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("invalid integer: %s", s)
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// updateAgentHeartbeat records a heartbeat timestamp on an agent bead via a
// heartbeat:EPOCH label. This proves the agent is alive during long idle periods.
//
// bd agent heartbeat was never shipped (steveyegge/beads#2828). We use the same
// read-modify-write label pattern as setAgentIdleCycles instead.
func updateAgentHeartbeat(agentBead, beadsDir string) error {
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	var newLabels []string
	for _, label := range allLabels {
		if len(label) > 10 && label[:10] == "heartbeat:" {
			continue // Replace existing heartbeat label
		}
		newLabels = append(newLabels, label)
	}
	newLabels = append(newLabels, fmt.Sprintf("heartbeat:%d", time.Now().Unix()))

	args := []string{"update", agentBead}
	for _, label := range newLabels {
		args = append(args, "--set-labels="+label)
	}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.MutationPinned, args...)
	return cmd.Run()
}

// setAgentIdleCycles sets the idle:N label on an agent bead.
// Uses read-modify-write pattern to update only the idle label.
func setAgentIdleCycles(agentBead, beadsDir string, cycles int) error {
	// Read all current labels
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	// Build new label list: keep non-idle labels, add new idle value
	var newLabels []string
	for _, label := range allLabels {
		// Skip any existing idle:* label
		if len(label) > 5 && label[:5] == "idle:" {
			continue
		}
		newLabels = append(newLabels, label)
	}

	// Add new idle value
	newLabels = append(newLabels, fmt.Sprintf("idle:%d", cycles))

	// Use bd update with --set-labels to replace all labels
	args := []string{"update", agentBead}
	for _, label := range newLabels {
		args = append(args, "--set-labels="+label)
	}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.MutationPinned, args...)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("setting idle label: %w", err)
	}

	return nil
}

// setAgentBackoffUntil persists a backoff-until:TIMESTAMP label on the agent bead.
// This allows interrupted await-signal invocations to resume with remaining time
// instead of restarting the full backoff period.
func setAgentBackoffUntil(agentBead, beadsDir string, until time.Time) error {
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	var newLabels []string
	for _, label := range allLabels {
		if len(label) > 14 && label[:14] == "backoff-until:" {
			continue // Strip existing backoff-until
		}
		newLabels = append(newLabels, label)
	}
	newLabels = append(newLabels, fmt.Sprintf("backoff-until:%d", until.Unix()))

	args := []string{"update", agentBead}
	for _, label := range newLabels {
		args = append(args, "--set-labels="+label)
	}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.MutationPinned, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("setting backoff-until label: %w", err)
	}
	return nil
}

// clearAgentBackoffUntil removes the backoff-until label from the agent bead.
// Called when await-signal completes normally (timeout or signal received).
func clearAgentBackoffUntil(agentBead, beadsDir string) error {
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	var newLabels []string
	found := false
	for _, label := range allLabels {
		if len(label) > 14 && label[:14] == "backoff-until:" {
			found = true
			continue // Strip backoff-until
		}
		newLabels = append(newLabels, label)
	}

	if !found {
		return nil // Nothing to clear
	}

	args := []string{"update", agentBead}
	if len(newLabels) == 0 {
		args = append(args, "--set-labels=")
	} else {
		for _, label := range newLabels {
			args = append(args, "--set-labels="+label)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.MutationPinned, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clearing backoff-until label: %w", err)
	}
	return nil
}

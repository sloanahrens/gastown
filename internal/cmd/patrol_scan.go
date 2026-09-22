package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/witness"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	patrolScanJSON              bool
	patrolScanNotify            bool
	patrolScanRig               string
	patrolScanVerbose           bool
	patrolScanActivityThreshold time.Duration
)

var patrolScanCmd = &cobra.Command{
	Use:   "scan",
	Short: "Scan polecats for zombies, stalls, and completions",
	Long: `Run proactive detection across all polecats in a rig.

This command bridges the witness library detection functions to the CLI,
providing a single command for the survey-workers patrol step.

Detections:
  - Zombies: Dead sessions with active agent state, dead agent processes,
    stuck done-intent, closed beads with live sessions
  - Stalls: Agents stuck at startup prompts
  - Refinery stall: A live refinery holding unsubmitted composer input while
    producing no output (gt-hkhu). Queued input is submitted on detection.
  - Completions: Agent bead metadata indicating gt done was called
  - Activity: Real work recency per live session, from the agent's transcript
    and pane content — NOT from the pane's rendered spinner/elapsed label

Actions taken automatically:
  - Zombie restart: Sessions are restarted (not nuked) to preserve worktrees
  - Cleanup wisps: Created for dirty state tracking
  - Completion routing: MR cleanup wisps created, refinery nudged

The activity section is report-only: it never restarts anything. It exists so
that a "hung session" judgement can be backed by evidence. To act on it, take
two scans at least --activity-threshold apart and compare the pane_signature
and last_activity_age_seconds of the same polecat; only if BOTH are unchanged
is there a positive stall signal (gt-xb27). A polecat deep inside one long turn
looks busy on every other signal and must be left alone.

Use --notify to send mail when zombies with active work are detected.
Long-running scan phases emit progress diagnostics to stderr so JSON stdout
remains machine-readable while operators can see where a slow patrol is stuck.

Examples:
  gt patrol scan                    # Scan current rig
  gt patrol scan --rig gastown      # Scan specific rig
  gt patrol scan --json             # Machine-readable output
  gt patrol scan --notify           # Send mail on zombie detection`,
	RunE: runPatrolScan,
}

func init() {
	patrolScanCmd.Flags().BoolVar(&patrolScanJSON, "json", false, "Output as JSON")
	patrolScanCmd.Flags().BoolVar(&patrolScanNotify, "notify", false, "Send mail to witness/mayor when active-work zombies are detected")
	patrolScanCmd.Flags().StringVar(&patrolScanRig, "rig", "", "Rig to scan (default: infer from cwd or GT_RIG)")
	patrolScanCmd.Flags().BoolVarP(&patrolScanVerbose, "verbose", "v", false, "Verbose output")
	patrolScanCmd.Flags().DurationVar(&patrolScanActivityThreshold, "activity-threshold", constants.HungSessionThreshold,
		"Age of last real activity that marks a polecat as a stall candidate (report-only)")

	patrolCmd.AddCommand(patrolScanCmd)
}

var patrolScanProgressInterval = 10 * time.Second

// PatrolScanOutput is the JSON output format for patrol scan results.
type PatrolScanOutput struct {
	Rig         string                    `json:"rig"`
	Timestamp   string                    `json:"timestamp"`
	Zombies     *PatrolScanZombieOutput   `json:"zombies"`
	Stalls      *PatrolScanStallOutput    `json:"stalls,omitempty"`
	Refinery    *PatrolScanRefineryOutput `json:"refinery,omitempty"`
	Completions *PatrolScanCompleteOutput `json:"completions,omitempty"`
	Activity    *PatrolScanActivityOutput `json:"activity,omitempty"`
	Receipts    []witness.PatrolReceipt   `json:"receipts,omitempty"`
}

// PatrolScanRefineryOutput holds the refinery composer-stall check (gt-hkhu).
type PatrolScanRefineryOutput struct {
	Checked int                      `json:"checked"`
	Found   int                      `json:"found"`
	Stalls  []PatrolScanRefineryItem `json:"stalls,omitempty"`
	Errors  []string                 `json:"errors,omitempty"`
}

// PatrolScanRefineryItem is a single refinery stall in scan output.
type PatrolScanRefineryItem struct {
	Session           string  `json:"session"`
	Agent             string  `json:"agent"`
	StallType         string  `json:"stall_type"`
	State             string  `json:"state"`
	InactivitySeconds float64 `json:"inactivity_seconds"`
	// PendingSeconds is how long the composer had been continuously observed
	// holding unsubmitted input. It is non-zero whenever an earlier probe
	// started the run, including when the silence window is what tripped this
	// verdict; it is zero only on the first observation, and it does NOT say
	// which clock fired (gt-afa7).
	PendingSeconds float64 `json:"pending_seconds"`
	// PendingSamples is how many consecutive observations have seen the
	// composer pending. The age only counts once several samples spanning a
	// minimum window agree, so this is what tells a run that was restarted
	// mid-flight from one that has been continuously unattended (gt-afa7).
	PendingSamples int    `json:"pending_samples"`
	Action         string `json:"action"`
	Error          string `json:"error,omitempty"`
}

// PatrolScanZombieOutput holds zombie detection results.
type PatrolScanZombieOutput struct {
	Checked int                    `json:"checked"`
	Found   int                    `json:"found"`
	Zombies []PatrolScanZombieItem `json:"zombies,omitempty"`
	Errors  []string               `json:"errors,omitempty"`
}

// PatrolScanZombieItem is a single zombie detection in scan output.
type PatrolScanZombieItem struct {
	Polecat        string `json:"polecat"`
	Classification string `json:"classification"`
	AgentState     string `json:"agent_state"`
	HookBead       string `json:"hook_bead,omitempty"`
	CleanupStatus  string `json:"cleanup_status,omitempty"`
	Action         string `json:"action"`
	WasActive      bool   `json:"was_active"`
	Error          string `json:"error,omitempty"`
}

// PatrolScanStallOutput holds stall detection results.
type PatrolScanStallOutput struct {
	Checked int                   `json:"checked"`
	Found   int                   `json:"found"`
	Stalls  []PatrolScanStallItem `json:"stalls,omitempty"`
}

// PatrolScanStallItem is a single stall detection in scan output.
type PatrolScanStallItem struct {
	Polecat   string `json:"polecat"`
	StallType string `json:"stall_type"`
	Action    string `json:"action"`
	Error     string `json:"error,omitempty"`
}

// PatrolScanCompleteOutput holds completion discovery results.
type PatrolScanCompleteOutput struct {
	Checked   int                      `json:"checked"`
	Found     int                      `json:"found"`
	Completed []PatrolScanCompleteItem `json:"completed,omitempty"`
}

// PatrolScanCompleteItem is a single completion discovery in scan output.
type PatrolScanCompleteItem struct {
	Polecat        string `json:"polecat"`
	ExitType       string `json:"exit_type"`
	IssueID        string `json:"issue_id,omitempty"`
	MRID           string `json:"mr_id,omitempty"`
	Branch         string `json:"branch,omitempty"`
	Action         string `json:"action"`
	WispCreated    string `json:"wisp_created,omitempty"`
	CompletionTime string `json:"completion_time,omitempty"`
}

// PatrolScanActivityOutput holds real-activity observations for live sessions.
// Report-only: no restart is ever driven by this section alone (gt-xb27).
type PatrolScanActivityOutput struct {
	Checked         int                      `json:"checked"`
	Threshold       string                   `json:"threshold"`
	StaleCandidates int                      `json:"stale_candidates"`
	Items           []PatrolScanActivityItem `json:"items,omitempty"`
}

// PatrolScanActivityItem is one live session's real-activity snapshot.
//
// last_activity_age_seconds and pane_signature are the two signals a caller may
// compare across scans. If a later scan shows a larger age AND an identical
// pane_signature for the same session, that is positive stall evidence.
// agent_alive must be true for a stall verdict to mean anything, and anything
// other than activity_source="transcript" means "last activity unknown" — a
// missing transcript is never evidence of a stall.
type PatrolScanActivityItem struct {
	Polecat                string   `json:"polecat"`
	Session                string   `json:"session"`
	AgentAlive             bool     `json:"agent_alive"`
	LastActivityAgeSeconds *float64 `json:"last_activity_age_seconds,omitempty"`
	ActivitySource         string   `json:"activity_source"`
	Transcript             string   `json:"transcript,omitempty"`
	TranscriptBytes        int64    `json:"transcript_bytes,omitempty"`
	PaneSignature          string   `json:"pane_signature,omitempty"`
	StallCandidate         bool     `json:"stall_candidate"`
	Note                   string   `json:"note,omitempty"`
	Errors                 []string `json:"errors,omitempty"`
}

func runPatrolScan(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Determine rig name
	rigName := patrolScanRig
	if rigName == "" {
		// Try GT_RIG env, then infer from cwd
		rigName = os.Getenv("GT_RIG")
		if rigName == "" {
			rigName, err = inferRigFromCwd(townRoot)
			if err != nil {
				return fmt.Errorf("could not determine rig: %w\nUse --rig to specify", err)
			}
		}
	}

	bd := witness.DefaultBdCli()
	router := mail.NewRouter(townRoot)
	workDir := townRoot

	timestamp := time.Now().UTC().Format(time.RFC3339)

	// Run all four detection passes.
	// Note: DetectZombiePolecats takes a router param but does NOT send mail
	// internally — it only uses the router for workspace context. Notifications
	// are sent exclusively below via --notify, avoiding double-send.
	diagnostics := cmd.ErrOrStderr()
	zombieResult := runPatrolScanPhase(diagnostics, "zombie detection", func() *witness.DetectZombiePolecatsResult {
		return witness.DetectZombiePolecats(bd, workDir, rigName, router)
	})
	stallResult := runPatrolScanPhase(diagnostics, "stall detection", func() *witness.DetectStalledPolecatsResult {
		return witness.DetectStalledPolecats(workDir, rigName)
	})
	// Separate from stall detection: the refinery is not a polecat and has no
	// heartbeat, so the polecat sweep never looks at it. A refinery holding a
	// composed-but-unsubmitted instruction reads as running to every other
	// check while MRs age behind it (gt-hkhu).
	refineryResult := runPatrolScanPhase(diagnostics, "refinery stall detection", func() *witness.DetectRefineryStallResult {
		return witness.DetectStalledRefinery(workDir, rigName)
	})
	completionResult := runPatrolScanPhase(diagnostics, "completion discovery", func() *witness.DiscoverCompletionsResult {
		return witness.DiscoverCompletions(bd, workDir, rigName, router)
	})
	// Observed last so it reflects the state after the automatic actions above.
	activityResult := runPatrolScanPhase(diagnostics, "real-activity observation", func() []witness.RealActivity {
		return witness.ObserveRigRealActivity(workDir, rigName)
	})

	// Build patrol receipts for zombies
	receipts := witness.BuildPatrolReceipts(rigName, zombieResult)

	// Notify when zombies with active work are detected.
	// Always notify the mayor for active-work zombies (dead polecats with hooked
	// beads) — this is the primary mechanism for detecting failed work. (GH #3584)
	// Use --notify=false to suppress (e.g., in dry-run/testing contexts).
	if zombieResult != nil {
		activeZombies := countActiveWorkZombies(zombieResult)
		if activeZombies > 0 {
			sendZombieNotification(router, rigName, zombieResult, activeZombies)
		}
	}

	if patrolScanJSON {
		return outputPatrolScanJSON(rigName, timestamp, zombieResult, stallResult, refineryResult, completionResult, activityResult, receipts)
	}

	return outputPatrolScanHuman(rigName, zombieResult, stallResult, refineryResult, completionResult, activityResult, receipts)
}

func runPatrolScanPhase[T any](diagnostics io.Writer, name string, fn func() T) T {
	start := time.Now()
	if diagnostics != nil {
		fmt.Fprintf(diagnostics, "gt patrol scan: starting %s\n", name)
	}

	done := make(chan T, 1)
	go func() {
		done <- fn()
	}()

	if patrolScanProgressInterval <= 0 {
		result := <-done
		if diagnostics != nil {
			fmt.Fprintf(diagnostics, "gt patrol scan: finished %s in %s\n", name, formatPatrolScanElapsed(time.Since(start)))
		}
		return result
	}

	ticker := time.NewTicker(patrolScanProgressInterval)
	defer ticker.Stop()

	for {
		select {
		case result := <-done:
			if diagnostics != nil {
				fmt.Fprintf(diagnostics, "gt patrol scan: finished %s in %s\n", name, formatPatrolScanElapsed(time.Since(start)))
			}
			return result
		case <-ticker.C:
			if diagnostics != nil {
				fmt.Fprintf(diagnostics, "gt patrol scan: still running %s after %s\n", name, formatPatrolScanElapsed(time.Since(start)))
			}
		}
	}
}

func formatPatrolScanElapsed(elapsed time.Duration) string {
	if elapsed < time.Second {
		return elapsed.Round(time.Millisecond).String()
	}
	return elapsed.Round(time.Second).String()
}

func countActiveWorkZombies(result *witness.DetectZombiePolecatsResult) int {
	count := 0
	for _, z := range result.Zombies {
		if z.WasActive {
			count++
		}
	}
	return count
}

func sendZombieNotification(router *mail.Router, rigName string, result *witness.DetectZombiePolecatsResult, activeCount int) {
	var lines []string
	lines = append(lines, fmt.Sprintf("Patrol scan detected %d zombie(s) with active work in rig %s:", activeCount, rigName))
	lines = append(lines, "")
	for _, z := range result.Zombies {
		if !z.WasActive {
			continue
		}
		line := fmt.Sprintf("- %s: %s (hook=%s, action=%s)",
			z.PolecatName, string(z.Classification), z.HookBead, z.Action)
		if z.Error != nil {
			line += fmt.Sprintf(" [error: %v]", z.Error)
		}
		lines = append(lines, line)
	}

	body := strings.Join(lines, "\n")
	subject := fmt.Sprintf("ZOMBIE_DETECTED: %d active-work zombie(s) in %s", activeCount, rigName)

	// Send to witness (best-effort)
	witMsg := &mail.Message{
		From:    fmt.Sprintf("%s/witness", rigName),
		To:      fmt.Sprintf("%s/witness", rigName),
		Subject: subject,
		Body:    body,
	}
	_ = router.Send(witMsg)

	// Also notify the mayor so dead polecats don't go unnoticed. (GH #3584)
	// The mayor needs to know so work can be reslung.
	mayorBody := strings.Join(lines, "\n") +
		"\n\nResling instructions:\n" +
		"  gt sling <bead-id> <rig> --create --force"
	mayorMsg := &mail.Message{
		From:    fmt.Sprintf("%s/witness", rigName),
		To:      "mayor/",
		Subject: fmt.Sprintf("POLECAT_DIED: %d polecat(s) died with active work in %s", activeCount, rigName),
		Body:    mayorBody,
	}
	_ = router.Send(mayorMsg)
}

func outputPatrolScanJSON(rigName, timestamp string, zombieResult *witness.DetectZombiePolecatsResult, stallResult *witness.DetectStalledPolecatsResult, refineryResult *witness.DetectRefineryStallResult, completionResult *witness.DiscoverCompletionsResult, activityResult []witness.RealActivity, receipts []witness.PatrolReceipt) error {
	output := PatrolScanOutput{
		Rig:       rigName,
		Timestamp: timestamp,
		Receipts:  receipts,
	}

	// Zombies
	if zombieResult != nil {
		zo := &PatrolScanZombieOutput{
			Checked: zombieResult.Checked,
			Found:   len(zombieResult.Zombies),
		}
		for _, z := range zombieResult.Zombies {
			item := PatrolScanZombieItem{
				Polecat:        z.PolecatName,
				Classification: string(z.Classification),
				AgentState:     z.AgentState,
				HookBead:       z.HookBead,
				CleanupStatus:  z.CleanupStatus,
				Action:         z.Action,
				WasActive:      z.WasActive,
			}
			if z.Error != nil {
				item.Error = z.Error.Error()
			}
			zo.Zombies = append(zo.Zombies, item)
		}
		for _, e := range zombieResult.Errors {
			zo.Errors = append(zo.Errors, e.Error())
		}
		output.Zombies = zo
	}

	// Stalls
	if stallResult != nil {
		so := &PatrolScanStallOutput{
			Checked: stallResult.Checked,
			Found:   len(stallResult.Stalled),
		}
		for _, s := range stallResult.Stalled {
			item := PatrolScanStallItem{
				Polecat:   s.PolecatName,
				StallType: s.StallType,
				Action:    s.Action,
			}
			if s.Error != nil {
				item.Error = s.Error.Error()
			}
			so.Stalls = append(so.Stalls, item)
		}
		output.Stalls = so
	}

	// Refinery composer stall
	if refineryResult != nil {
		ro := &PatrolScanRefineryOutput{
			Checked: refineryResult.Checked,
			Found:   len(refineryResult.Stalls),
		}
		for _, s := range refineryResult.Stalls {
			item := PatrolScanRefineryItem{
				Session:           s.Session,
				Agent:             s.Agent,
				StallType:         s.StallType,
				State:             s.State,
				InactivitySeconds: s.Inactivity.Seconds(),
				PendingSeconds:    s.PendingFor.Seconds(),
				PendingSamples:    s.PendingSamples,
				Action:            s.Action,
			}
			if s.Error != nil {
				item.Error = s.Error.Error()
			}
			ro.Stalls = append(ro.Stalls, item)
		}
		for _, e := range refineryResult.Errors {
			ro.Errors = append(ro.Errors, e.Error())
		}
		output.Refinery = ro
	}

	// Completions
	if completionResult != nil {
		co := &PatrolScanCompleteOutput{
			Checked: completionResult.Checked,
			Found:   len(completionResult.Discovered),
		}
		for _, d := range completionResult.Discovered {
			item := PatrolScanCompleteItem{
				Polecat:        d.PolecatName,
				ExitType:       d.ExitType,
				IssueID:        d.IssueID,
				MRID:           d.MRID,
				Branch:         d.Branch,
				Action:         d.Action,
				WispCreated:    d.WispCreated,
				CompletionTime: d.CompletionTime,
			}
			co.Completed = append(co.Completed, item)
		}
		output.Completions = co
	}

	// Activity
	output.Activity = buildActivityOutput(activityResult, time.Now())

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

// buildActivityOutput projects real-activity snapshots into scan output.
// now is a parameter so callers and tests agree on the reference instant.
func buildActivityOutput(observed []witness.RealActivity, now time.Time) *PatrolScanActivityOutput {
	out := &PatrolScanActivityOutput{
		Checked:   len(observed),
		Threshold: patrolScanActivityThreshold.String(),
	}
	for _, act := range observed {
		item := PatrolScanActivityItem{
			Polecat:         act.Polecat,
			Session:         act.Session,
			AgentAlive:      act.AgentAlive,
			ActivitySource:  act.ActivitySource,
			Transcript:      act.TranscriptPath,
			TranscriptBytes: act.TranscriptBytes,
			PaneSignature:   act.PaneSignature,
			StallCandidate:  act.IsStaleCandidate(now, patrolScanActivityThreshold),
			Errors:          act.Errors,
		}
		if age, ok := act.Age(now); ok {
			seconds := age.Seconds()
			item.LastActivityAgeSeconds = &seconds
		}
		if item.StallCandidate {
			item.Note = "candidate only — re-scan after the full threshold and require an unchanged pane_signature before any restart"
		}
		out.Items = append(out.Items, item)
		if item.StallCandidate {
			out.StaleCandidates++
		}
	}
	return out
}

func outputPatrolScanHuman(rigName string, zombieResult *witness.DetectZombiePolecatsResult, stallResult *witness.DetectStalledPolecatsResult, refineryResult *witness.DetectRefineryStallResult, completionResult *witness.DiscoverCompletionsResult, activityResult []witness.RealActivity, _ []witness.PatrolReceipt) error {
	fmt.Printf("%s Patrol scan: %s\n\n", style.Bold.Render("🔍"), rigName)

	// Zombies
	if zombieResult != nil {
		fmt.Printf("%s Zombie Detection: checked %d polecat(s)\n",
			style.Bold.Render("👻"), zombieResult.Checked)

		if len(zombieResult.Zombies) == 0 {
			fmt.Printf("  %s\n", style.Dim.Render("No zombies detected"))
		} else {
			for _, z := range zombieResult.Zombies {
				icon := "⚠"
				if z.WasActive {
					icon = "🚨"
				}
				fmt.Printf("  %s %s: %s\n", icon, z.PolecatName, z.Classification)
				fmt.Printf("    State: %s", z.AgentState)
				if z.HookBead != "" {
					fmt.Printf("  Hook: %s", z.HookBead)
				}
				if z.CleanupStatus != "" {
					fmt.Printf("  Cleanup: %s", z.CleanupStatus)
				}
				fmt.Println()
				fmt.Printf("    Action: %s\n", z.Action)
				if z.Error != nil {
					fmt.Printf("    %s\n", style.Dim.Render(fmt.Sprintf("Error: %v", z.Error)))
				}
			}
		}

		if len(zombieResult.Errors) > 0 && patrolScanVerbose {
			fmt.Printf("  Errors: %d\n", len(zombieResult.Errors))
			for _, e := range zombieResult.Errors {
				fmt.Printf("    - %v\n", e)
			}
		}

		if len(zombieResult.ConvoyFailures) > 0 {
			fmt.Printf("  Convoy failures: %d\n", len(zombieResult.ConvoyFailures))
		}
		fmt.Println()
	}

	// Stalls
	if stallResult != nil && (len(stallResult.Stalled) > 0 || patrolScanVerbose) {
		fmt.Printf("%s Stall Detection: checked %d polecat(s)\n",
			style.Bold.Render("⏳"), stallResult.Checked)

		if len(stallResult.Stalled) == 0 {
			fmt.Printf("  %s\n", style.Dim.Render("No stalls detected"))
		} else {
			for _, s := range stallResult.Stalled {
				fmt.Printf("  ⚠ %s: %s → %s\n", s.PolecatName, s.StallType, s.Action)
				if s.Error != nil {
					fmt.Printf("    %s\n", style.Dim.Render(fmt.Sprintf("Error: %v", s.Error)))
				}
			}
		}
		fmt.Println()
	}

	// Refinery composer stall (gt-hkhu)
	if refineryResult != nil && (len(refineryResult.Stalls) > 0 || patrolScanVerbose) {
		fmt.Printf("%s Refinery Stall Detection: checked %d session(s)\n",
			style.Bold.Render("🏭"), refineryResult.Checked)

		if len(refineryResult.Stalls) == 0 {
			fmt.Printf("  %s\n", style.Dim.Render("No composer stalls detected"))
		} else {
			for _, s := range refineryResult.Stalls {
				// Report the age only when there is one. On a first
				// observation it is zero, and "input waiting 0s" reads as input
				// that just arrived rather than as the silence window having
				// tripped — the opposite of what happened (gt-afa7).
				clocks := fmt.Sprintf("silent %s", s.Inactivity.Round(time.Second))
				if s.PendingFor > 0 {
					clocks += fmt.Sprintf(", input waiting %s over %d observation(s)",
						s.PendingFor.Round(time.Second), s.PendingSamples)
				}
				fmt.Printf("  ⚠ %s: %s (composer %s, %s) → %s\n",
					s.Agent, s.StallType, s.State, clocks, s.Action)
				if s.Error != nil {
					fmt.Printf("    %s\n", style.Dim.Render(fmt.Sprintf("Error: %v", s.Error)))
				}
			}
		}
		if len(refineryResult.Errors) > 0 && patrolScanVerbose {
			for _, e := range refineryResult.Errors {
				fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("Error: %v", e)))
			}
		}
		fmt.Println()
	}

	// Completions
	if completionResult != nil && (len(completionResult.Discovered) > 0 || patrolScanVerbose) {
		fmt.Printf("%s Completion Discovery: checked %d polecat(s)\n",
			style.Bold.Render("✅"), completionResult.Checked)

		if len(completionResult.Discovered) == 0 {
			fmt.Printf("  %s\n", style.Dim.Render("No completions discovered"))
		} else {
			for _, d := range completionResult.Discovered {
				fmt.Printf("  ● %s: exit=%s", d.PolecatName, d.ExitType)
				if d.IssueID != "" {
					fmt.Printf("  issue=%s", d.IssueID)
				}
				if d.MRID != "" {
					fmt.Printf("  mr=%s", d.MRID)
				}
				fmt.Println()
				fmt.Printf("    Action: %s\n", d.Action)
			}
		}
		fmt.Println()
	}

	// Activity (report-only — never a restart trigger on its own)
	now := time.Now()
	if len(activityResult) > 0 {
		fmt.Printf("%s Real Activity: %d live session(s), threshold %s\n",
			style.Bold.Render("⏱"), len(activityResult), patrolScanActivityThreshold)

		for _, act := range activityResult {
			candidate := act.IsStaleCandidate(now, patrolScanActivityThreshold)
			icon := "●"
			if candidate {
				icon = "⚠"
			}
			fmt.Printf("  %s %s: %s\n", icon, act.Polecat, act.Describe(now))
			if candidate {
				fmt.Printf("      %s\n", style.Dim.Render(
					"candidate only — re-scan after the full threshold and require an unchanged pane signature before any restart"))
			}
			if len(act.Errors) > 0 && patrolScanVerbose {
				for _, e := range act.Errors {
					fmt.Printf("      %s\n", style.Dim.Render(e))
				}
			}
		}
		fmt.Println()
	}

	// Summary
	zombieCount := 0
	activeCount := 0
	if zombieResult != nil {
		zombieCount = len(zombieResult.Zombies)
		activeCount = countActiveWorkZombies(zombieResult)
	}
	stallCount := 0
	if stallResult != nil {
		stallCount = len(stallResult.Stalled)
	}
	completionCount := 0
	if completionResult != nil {
		completionCount = len(completionResult.Discovered)
	}
	staleCandidates := 0
	for _, act := range activityResult {
		if act.IsStaleCandidate(now, patrolScanActivityThreshold) {
			staleCandidates++
		}
	}
	refineryStallCount := 0
	if refineryResult != nil {
		refineryStallCount = len(refineryResult.Stalls)
	}

	if zombieCount == 0 && stallCount == 0 && refineryStallCount == 0 && completionCount == 0 {
		fmt.Printf("%s All clear — no issues detected\n", style.Success.Render("✓"))
	} else {
		fmt.Printf("Summary: %d zombie(s) (%d active-work), %d stall(s), %d refinery stall(s), %d completion(s), %d activity candidate(s)\n",
			zombieCount, activeCount, stallCount, refineryStallCount, completionCount, staleCandidates)
	}

	return nil
}

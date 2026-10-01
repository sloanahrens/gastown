// Emergency stop (gt estop / gt thaw): stop and resume dispatch and
// restarts. Running sessions are never signaled (gt-4k3fj.4): the SIGTSTP
// freeze this used to send is gone, since tmux SIGCONTs a stopped pane.
//
// Original implementation by outdoorsea (PR #3237). Cherry-picked for
// manual-only operation: no daemon auto-trigger.
package cmd

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	estopReason string
	estopRig    string
	thawRig     string
)

var estopCmd = &cobra.Command{
	Use:     "estop",
	GroupID: GroupServices,
	Short:   "Emergency stop: no new dispatch, no restarts",
	Long: `Emergency stop for the whole town (or a single rig).

An E-stop writes a sentinel file at the town root (ESTOP, or ESTOP.<rig>
with --rig). While it is present:

  - nothing new is dispatched: the daemon's dispatchers (scheduler, convoy
    feeder, scheduled slings, spec dispatcher, seat refill) hold, and
    gt sling refuses to send work into a covered rig;
  - nothing is restarted or killed: the supervisor refuses every Restart
    and Kill for a covered seat, whoever asks.

Running sessions are left alone: they finish their current work or go
idle. Nothing is signaled, frozen or killed. Agents see the E-stop in
their mail-check reminder and are told to checkpoint and wait.

To end running sessions as well, use gt kill-all.

Use --rig to stop a single rig instead of the whole town.

To resume: gt thaw [--rig <name>]

Examples:
  gt estop                              # Stop the whole town
  gt estop -r "closing laptop"          # Stop with a reason
  gt estop --rig gastown                # Stop only gastown
  gt estop status                       # Show E-stop state`,
	// Reject stray operands so `+"`gt estop status`"+` cannot fall through to runEstop.
	Args: cobra.NoArgs,
	RunE: runEstop,
}

var estopStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show emergency-stop state",
	Long:  "Report whether a town-wide or per-rig E-stop is active. This is read-only. To clear an E-stop, use 'gt thaw'.",
	Args:  cobra.NoArgs,
	RunE:  runEstopStatus,
}

func runEstopStatus(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	estopStatus(cmd.OutOrStdout(), townRoot)
	return nil
}

// estopStatus reports the town-wide and per-rig E-stops under townRoot to w.
func estopStatus(w io.Writer, townRoot string) {
	if !estop.IsActive(townRoot) {
		entries, _ := filepath.Glob(filepath.Join(townRoot, "ESTOP.*"))
		hasRigEstop := false
		for _, entry := range entries {
			rigName := strings.TrimPrefix(filepath.Base(entry), "ESTOP.")
			if estop.ReadRig(townRoot, rigName) != nil {
				hasRigEstop = true
				break
			}
		}
		if !hasRigEstop {
			fmt.Fprintln(w, "No E-stop active.")
			return
		}
	}
	addEstopToStatus(w, townRoot)
	fmt.Fprintf(w, "Clear with: %s\n", style.Bold.Render("gt thaw"))
}

var thawCmd = &cobra.Command{
	Use:     "thaw",
	GroupID: GroupServices,
	Short:   "Clear an emergency stop so dispatch and restarts resume",
	Long: `Clear an E-stop set by gt estop.

Removes the ESTOP sentinel file (or ESTOP.<rig> with --rig), so dispatch
and supervisor restarts resume, and nudges the covered sessions that work
may continue.

Examples:
  gt thaw                    # Clear the town-wide E-stop
  gt thaw --rig gastown      # Clear only gastown's E-stop`,
	Args: cobra.NoArgs,
	RunE: runThaw,
}

func init() {
	estopCmd.Flags().StringVarP(&estopReason, "reason", "r", "", "Reason for the E-stop")
	estopCmd.Flags().StringVar(&estopRig, "rig", "", "Stop only this rig (instead of the whole town)")
	thawCmd.Flags().StringVar(&thawRig, "rig", "", "Clear only this rig's E-stop")
	estopCmd.AddCommand(estopStatusCmd)
	rootCmd.AddCommand(estopCmd)
	rootCmd.AddCommand(thawCmd)
}

func runEstop(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return activateEstop(cmd.OutOrStdout(), townRoot, estopRig, estopReason)
}

// activateEstop writes the town (rig == "") or per-rig ESTOP sentinel and
// reports it to w. It touches no session: the sentinel alone is what the
// dispatch hold and the supervisor read.
func activateEstop(w io.Writer, townRoot, rig, reason string) error {
	if rig != "" {
		if info := estop.ReadRig(townRoot, rig); info != nil {
			fmt.Fprintf(w, "%s E-stop already active for %s (triggered %s: %s)\n",
				style.Error.Render("⛔"), rig, info.Trigger, info.Reason)
			return nil
		}
		if err := estop.ActivateRig(townRoot, rig, estop.TriggerManual, reason); err != nil {
			return fmt.Errorf("failed to create ESTOP file for %s: %w", rig, err)
		}
		fmt.Fprintf(w, "%s EMERGENCY STOP: %s\n", style.Error.Render("⛔"), style.Bold.Render(rig))
	} else {
		if info := estop.Read(townRoot); info != nil {
			fmt.Fprintf(w, "%s E-stop already active (triggered %s: %s)\n",
				style.Error.Render("⛔"), info.Trigger, info.Reason)
			return nil
		}
		if err := estop.Activate(townRoot, estop.TriggerManual, reason); err != nil {
			return fmt.Errorf("failed to create ESTOP file: %w", err)
		}
		fmt.Fprintf(w, "%s EMERGENCY STOP\n", style.Error.Render("⛔"))
	}
	if reason != "" {
		fmt.Fprintf(w, "   Reason: %s\n", reason)
	}
	fmt.Fprintln(w, "   No new dispatch, no restarts or kills; running sessions finish or idle.")
	fmt.Fprintf(w, "   End running sessions too: %s\n", style.Bold.Render(withRigFlag("gt kill-all", rig)))
	fmt.Fprintf(w, "   Resume with: %s\n", style.Bold.Render(withRigFlag("gt thaw", rig)))
	return nil
}

// withRigFlag appends --rig <rig> to a command line when rig is set.
func withRigFlag(cmdline, rig string) string {
	if rig == "" {
		return cmdline
	}
	return cmdline + " --rig " + rig
}

func runThaw(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	var info *estop.Info
	if thawRig != "" {
		info = estop.ReadRig(townRoot, thawRig)
	} else {
		info = estop.Read(townRoot)
	}
	if info == nil {
		if thawRig != "" {
			fmt.Printf("No E-stop active for %s.\n", thawRig)
		} else {
			fmt.Println("No E-stop active.")
		}
		return nil
	}

	if thawRig != "" {
		err = estop.DeactivateRig(townRoot, thawRig)
	} else {
		err = estop.Deactivate(townRoot, false)
	}
	if err != nil {
		return fmt.Errorf("failed to remove ESTOP file: %w", err)
	}
	scope := ""
	if thawRig != "" {
		scope = " for " + thawRig
	}
	fmt.Printf("%s E-stop cleared%s (was active for %s)\n", style.Success.Render("✓"),
		scope, time.Since(info.Timestamp).Round(time.Second))

	if t := tmux.NewTmux(); t.IsAvailable() {
		if nudged := nudgeAllSessions(townRegistry(), t, townRoot, thawRig); nudged > 0 {
			fmt.Printf("   Nudged %d session(s)\n", nudged)
		}
	}
	return nil
}

// exemptSessions are not nudged when an E-stop clears: they coordinate the
// stop rather than wait it out.
var exemptSessions = map[string]bool{
	session.MayorSessionName():    true,
	session.OverseerSessionName(): true,
}

// nudgeAllSessions sends a nudge to all GT sessions to alert them of resume.
// If rigFilter is non-empty, only sessions for that rig are nudged.
func nudgeAllSessions(reg *session.PrefixRegistry, t *tmux.Tmux, townRoot string, rigFilter string) int {
	sessions := collectGTSessions(reg, t, townRoot)
	nudged := 0

	var rigPrefix string
	if rigFilter != "" {
		rigPrefix = reg.PrefixForRig(rigFilter)
	}

	for _, sess := range sessions {
		if exemptSessions[sess] {
			continue
		}
		if rigFilter != "" && !isRigSession(sess, rigPrefix) {
			continue
		}
		if err := t.NudgeSession(sess, "E-stop cleared. Work may resume."); err == nil {
			nudged++
		}
	}

	return nudged
}

// isRigSession checks if a session name belongs to a specific rig prefix.
func isRigSession(name, rigPrefix string) bool {
	return strings.HasPrefix(name, rigPrefix+"-") || name == rigPrefix
}

// collectGTSessions returns all Gas Town tmux sessions.
func collectGTSessions(reg *session.PrefixRegistry, t *tmux.Tmux, townRoot string) []string {
	allSessions, err := t.ListSessions()
	if err != nil {
		return nil
	}

	rigs := discoverRigs(townRoot)
	prefixes := make(map[string]bool)
	for _, rigName := range rigs {
		prefixes[reg.PrefixForRig(rigName)] = true
	}

	var gtSessions []string
	for _, sess := range allSessions {
		if isGTSession(sess, prefixes) {
			gtSessions = append(gtSessions, sess)
		}
	}
	return gtSessions
}

// isGTSession checks if a session name belongs to Gas Town.
func isGTSession(name string, rigPrefixes map[string]bool) bool {
	// Town-level sessions (hq-*)
	if strings.HasPrefix(name, session.HQPrefix) {
		return true
	}

	// Rig-level sessions: <prefix>-witness, <prefix>-refinery,
	// <prefix>-crew-<name>, <prefix>-<polecat-name>
	for prefix := range rigPrefixes {
		if strings.HasPrefix(name, prefix+"-") || name == prefix {
			return true
		}
	}

	return false
}

// addEstopToStatus checks for E-stop and prints a banner if active.
// Called from gt status to surface E-stop state.
func addEstopToStatus(w io.Writer, townRoot string) {
	if estop.IsActive(townRoot) {
		info := estop.Read(townRoot)
		if info != nil {
			age := time.Since(info.Timestamp).Round(time.Second)
			fmt.Fprintf(w, "%s  E-STOP ACTIVE (%s, %s ago", style.Error.Render("⛔"), info.Trigger, age)
			if info.Reason != "" {
				fmt.Fprintf(w, ": %s", info.Reason)
			}
			fmt.Fprintln(w, ")")
			fmt.Fprintln(w)
		}
	}

	// Check for per-rig E-stops
	entries, _ := filepath.Glob(filepath.Join(townRoot, "ESTOP.*"))
	for _, entry := range entries {
		rigName := strings.TrimPrefix(filepath.Base(entry), "ESTOP.")
		info := estop.ReadRig(townRoot, rigName)
		if info != nil {
			age := time.Since(info.Timestamp).Round(time.Second)
			fmt.Fprintf(w, "%s  E-STOP: %s (%s, %s ago", style.Error.Render("⏸"), rigName, info.Trigger, age)
			if info.Reason != "" {
				fmt.Fprintf(w, ": %s", info.Reason)
			}
			fmt.Fprintln(w, ")")
		}
	}
	if len(entries) > 0 {
		fmt.Fprintln(w)
	}
}

// addPausedToStatus prints a banner for every agent currently paused via
// `gt agent pause` (gt-ahik). File layer only — cheap, no Dolt: one walk
// of .runtime/agents, and the marker path names the agent.
//
// Writes to w (the status writer) rather than stdout so the banner is
// capturable in tests — an unnamed "PAUSED" line was the original defect.
func addPausedToStatus(w io.Writer, townRoot string) {
	paused := agentpause.ListPaused(townRoot)
	for _, st := range paused {
		age := time.Since(st.PausedAt).Round(time.Second)
		who := firstOr(st.Address, "unknown agent")
		fmt.Fprintf(w, "%s  PAUSED %s (by %s, %s ago: %s)\n",
			style.Warning.Render("⏸"), style.Bold.Render(who),
			firstOr(st.PausedBy, "?"), age, agentpause.Reason(st))
	}
	if len(paused) > 0 {
		fmt.Fprintln(w)
	}
}

func firstOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

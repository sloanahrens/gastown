package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/workspace"
)

// pendingWorkGracePeriod bounds how recently the branch's last commit must
// have landed before polecat-stop-check treats the polecat as still
// mid-workflow rather than abandoned. Claude Code's Stop event fires at the
// end of every assistant turn, not only when a session actually ends
// (gt-couv) — a turn that ends because the agent kicked off a background
// verification step (build/lint/test) looks identical, from this command's
// point of view, to an idle polecat that forgot to call gt done. A polecat
// that just committed is overwhelmingly likely to still be running its
// formula's post-commit steps, not gone.
const pendingWorkGracePeriod = 2 * time.Minute

var tapPolecatStopCmd = &cobra.Command{
	Use:   "polecat-stop-check",
	Short: "Auto-run gt done on session Stop if polecat has pending work",
	Long: `Safety net for the "idle polecat" problem: polecats that finish work
but forget to call gt done before the session ends.

This command is designed to run from a Claude Code Stop hook. Stop fires at
the end of every assistant turn, not only when a session truly ends, so it
also checks:
1. Whether this is a polecat session (GT_POLECAT env var)
2. Whether gt done has already run (heartbeat state is "exiting" or "idle")
3. Whether the polecat has commits, stashes, or non-runtime dirty work
4. Whether the polecat's own slot-wrapped verification suite is still
   running, or its last commit landed within the pending-work grace period —
   either means this Stop event is a turn boundary, not abandonment

If the polecat has pending work that wasn't submitted, and neither of the
turn-boundary signals above applies, this command runs gt done to submit it.
If gt done already ran or there's nothing to submit, it exits silently.

Exit codes:
  0 - No action needed (not a polecat, already done, or gt done succeeded)
  1 - gt done was attempted but failed`,
	RunE:         runTapPolecatStop,
	SilenceUsage: true,
}

func init() {
	tapCmd.AddCommand(tapPolecatStopCmd)
}

func runTapPolecatStop(cmd *cobra.Command, args []string) error {
	// Only applies to polecats
	polecatName := os.Getenv("GT_POLECAT")
	if polecatName == "" {
		return nil // Not a polecat session — nothing to do
	}

	sessionName := os.Getenv("GT_SESSION")
	if sessionName == "" {
		return nil // No session tracking — can't check state
	}

	// Find town root for heartbeat check
	townRoot, _, _ := workspace.FindFromCwdWithFallback()
	if townRoot == "" {
		townRoot = os.Getenv("GT_TOWN_ROOT")
	}
	if townRoot == "" {
		return nil // Can't find workspace — exit quietly
	}

	// Check heartbeat state: if already "exiting" or "idle", gt done already ran
	hb := polecat.ReadSessionHeartbeat(townRoot, sessionName)
	if hb != nil {
		state := hb.EffectiveState()
		if state == polecat.HeartbeatExiting || state == polecat.HeartbeatIdle {
			return nil // gt done already ran or polecat is idle — nothing to do
		}
	}

	// Check if the polecat is on a feature branch with work to submit.
	rigName := os.Getenv("GT_RIG")
	if rigName == "" {
		return nil
	}

	// Reconstruct polecat worktree path
	polecatDir := filepath.Join(townRoot, rigName, "polecats", polecatName)
	// Try the nested clone layout first (polecats/<name>/<rig>/)
	cloneDir := filepath.Join(polecatDir, rigName)
	if _, err := os.Stat(filepath.Join(cloneDir, ".git")); err != nil {
		// Fall back to flat layout
		cloneDir = polecatDir
		if _, err := os.Stat(filepath.Join(cloneDir, ".git")); err != nil {
			return nil // No git repo found — exit quietly
		}
	}

	// Check current branch — skip if on main/master
	branchCmd := exec.Command("git", "-C", cloneDir, "rev-parse", "--abbrev-ref", "HEAD")
	branchOut, err := branchCmd.Output()
	if err != nil {
		return nil // Can't determine branch — exit quietly
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch == "main" || branch == "master" || branch == "HEAD" {
		return nil // On default branch — nothing to submit
	}

	pending, reason, err := polecatStopPendingWork(cloneDir, branch)
	if err != nil || !pending {
		return nil // Can't check, or no work to submit — don't block session stop
	}

	// This Stop event may just mark a turn boundary, not a real session end
	// (gt-couv): the polecat can still be legitimately waiting on a
	// background verification step. Two independent turn-end signals veto
	// the auto-run in that case; either is enough to hold off.
	if busy, busyReason := polecatStopVerificationRunning(townRoot, rigName, polecatName); busy {
		fmt.Fprintf(os.Stderr, "polecat-stop-check: %s — deferring gt done\n", busyReason)
		return nil
	}
	if recent, commitErr := polecatStopCommittedWithinGrace(cloneDir); commitErr == nil && recent {
		fmt.Fprintf(os.Stderr, "polecat-stop-check: last commit on %s is under %s old — deferring gt done\n", branch, pendingWorkGracePeriod)
		return nil
	}

	// Polecat has pending work! Run gt done as a safety net.
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "⚠️  Polecat %s has pending work on branch %s (%s)\n", polecatName, branch, reason)
	fmt.Fprintf(os.Stderr, "   Auto-running gt done as safety net...\n")
	fmt.Fprintf(os.Stderr, "\n")

	// Find gt binary path
	gtBin, err := os.Executable()
	if err != nil {
		gtBin = "gt"
	}

	// Run gt done in the polecat's worktree context
	doneCmd := exec.Command(gtBin, "done")
	doneCmd.Dir = cloneDir
	doneCmd.Stdout = os.Stdout
	doneCmd.Stderr = os.Stderr
	// Inherit environment (GT_POLECAT, GT_RIG, etc. are already set)
	doneCmd.Env = os.Environ()

	if err := doneCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Auto gt done failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "   Witness will handle cleanup.\n")
		// Don't return error — don't block session stop
		return nil
	}

	return nil
}

func polecatStopPendingWork(cloneDir, branch string) (bool, string, error) {
	g := git.NewGit(cloneDir)
	workStatus, err := g.CheckUncommittedWork()
	if err != nil {
		return false, "", err
	}

	if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingRuntime() {
		return true, fmt.Sprintf("%d non-runtime dirty file(s)", len(workStatus.NonRuntimePaths())), nil
	}
	if workStatus.StashCount > 0 {
		return true, fmt.Sprintf("%d branch stash(es)", workStatus.StashCount), nil
	}

	targetStatus, err := g.BranchTargetStatus(branch, "origin", nil)
	if err != nil {
		return false, "", err
	}
	if !targetStatus.Preserved && targetStatus.UnpreservedPatchCount > 0 {
		return true, fmt.Sprintf("%d unsubmitted commit(s)", targetStatus.UnpreservedPatchCount), nil
	}

	return false, "", nil
}

// polecatStopVerificationRunning reports whether this polecat currently
// holds the town-level container-gate slot (see internal/slot) — i.e. its
// own slot-wrapped build/test suite is still running in the background.
// A held slot whose owner role doesn't match this polecat is some other
// rig's suite and says nothing about this polecat's state, so it is not
// treated as busy here.
func polecatStopVerificationRunning(townRoot, rigName, polecatName string) (bool, string) {
	rep, err := slot.Status(townRoot)
	if err != nil || !rep.Held || rep.Owner == nil {
		return false, ""
	}
	if rep.Owner.Role != rigName+"/"+polecatName {
		return false, ""
	}
	return true, fmt.Sprintf("container-gate slot held by %s (verification suite running)", rep.Owner.Role)
}

// polecatStopCommittedWithinGrace reports whether the branch's most recent
// commit landed less than pendingWorkGracePeriod ago.
func polecatStopCommittedWithinGrace(cloneDir string) (bool, error) {
	out, err := exec.Command("git", "-C", cloneDir, "log", "-1", "--format=%ct").Output()
	if err != nil {
		return false, err
	}
	tsStr := strings.TrimSpace(string(out))
	if tsStr == "" {
		return false, nil
	}
	unixSeconds, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false, err
	}
	return time.Since(time.Unix(unixSeconds, 0)) < pendingWorkGracePeriod, nil
}

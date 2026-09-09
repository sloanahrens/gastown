package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// fetchTimeout bounds the default-branch refresh below so an unreachable
// remote can't hang the whole doctor scan (gt-ftt is the precedent incident
// for this exact failure shape in a different patrol-loop check).
const fetchTimeout = 30 * time.Second

// StalledPolecatCheck detects polecats whose tmux sessions have died but whose
// worktrees still contain unpushed commits. These are the most dangerous failure
// mode after disk space exhaustion: the polecat appears dead, and nuking it
// would permanently lose the committed work on its branch.
//
// This check warns about at-risk branches so they can be pushed before cleanup.
//
// "At risk" is decided by content, not by branch/ancestry alone (gt-4vbn): a
// branch whose commits landed on the default branch under a different SHA —
// via a rebase-merge, or a reimplementation by another polecat on another
// bead — is not at risk even though the exact branch is gone from origin.
// git.BranchTargetStatus answers that with layered evidence (ancestor,
// merge-tree no-op for rebase-merges, cherry patch-equivalence), and a
// terminal (closed/tombstoned) bead behind the branch is a second,
// independent signal that the work is resolved. Neither test alone is
// exhaustive — a fresh reimplementation with no shared history and an
// intentionally-reopened bead can still slip through — but together they
// remove the two failure modes that made this check fire forever on
// superseded work.
type StalledPolecatCheck struct {
	FixableCheck
	stalledPolecats []stalledPolecatInfo // Cached during Run for use in Fix

	// beadStatus looks up a bead's status ("open", "closed", ...). Overridable
	// in tests to avoid depending on a live bd binary/Dolt server.
	beadStatus func(workDir, beadID string) (status string, ok bool)
}

type stalledPolecatInfo struct {
	name          string
	rigName       string
	branch        string
	unpushedCount int
	clonePath     string
}

// NewStalledPolecatCheck creates a new stalled polecat check.
func NewStalledPolecatCheck() *StalledPolecatCheck {
	return &StalledPolecatCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "stalled-polecats",
				CheckDescription: "Detect polecats with dead sessions and unpushed work",
				CheckCategory:    CategoryCleanup,
			},
		},
		beadStatus: lookupBeadStatus,
	}
}

// Run checks all rigs for polecats with dead sessions and unpushed commits.
func (c *StalledPolecatCheck) Run(ctx *CheckContext) *CheckResult {
	t := tmux.NewTmux()
	var stalled []stalledPolecatInfo
	var checked int

	// Iterate over all rigs (or single rig if specified)
	rigsToCheck := c.findRigs(ctx)
	for _, rigName := range rigsToCheck {
		polecatsDir := filepath.Join(ctx.TownRoot, rigName, "polecats")
		entries, err := os.ReadDir(polecatsDir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}

			polecatName := entry.Name()
			checked++

			// Check if tmux session is alive
			sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
			alive, err := t.HasSession(sessionName)
			if err != nil || alive {
				continue // Session alive or can't check — skip
			}

			// Session is dead. Check for unpushed commits.
			clonePath := c.resolveClonePath(ctx.TownRoot, rigName, polecatName)
			if clonePath == "" {
				continue
			}

			polecatGit := git.NewGit(clonePath)
			branch, brErr := polecatGit.CurrentBranch()
			if brErr != nil || branch == "" {
				continue
			}

			// The comparison below trusts the worktree's local view of
			// origin/<default-branch>, which a crashed polecat's clone may
			// not have refreshed in a long time. Best-effort and bounded: a
			// dead polecat's remote may be genuinely unreachable, and a
			// stale view only over-flags (safe direction), never masks real
			// risk — but an unbounded fetch could hang the whole scan.
			_ = polecatGit.FetchDefaultBranchWithTimeout("origin", fetchTimeout)

			// BranchTargetStatus (not BranchPushedToRemote) asks "is this WORK
			// on the default branch", not "is this exact branch on origin":
			// it checks ancestry, rebase-merge equivalence (merge-tree
			// no-op), and patch-equivalence (cherry) against the default
			// branch, and — unlike BranchPreservationStatus — it does not
			// treat a polecat branch's own (possibly stale) upstream config
			// as evidence, so a locally-cached tracking ref for an
			// already-deleted origin branch can't mask a real comparison
			// against main (gt-4vbn).
			targetStatus, checkErr := polecatGit.BranchTargetStatus(branch, "origin", nil)
			if checkErr != nil || targetStatus.Preserved {
				continue // Already landed on the default branch, or can't check
			}

			if c.branchSupersededByClosedBead(clonePath, branch) {
				continue // Bead behind this branch is closed — work is resolved, not at risk
			}

			stalled = append(stalled, stalledPolecatInfo{
				name:          polecatName,
				rigName:       rigName,
				branch:        branch,
				unpushedCount: targetStatus.UnpreservedPatchCount,
				clonePath:     clonePath,
			})
		}
	}

	c.stalledPolecats = stalled

	if len(stalled) == 0 {
		msg := "No stalled polecats with unpushed work"
		if checked > 0 {
			msg = fmt.Sprintf("Checked %d polecat(s), no unpushed work at risk", checked)
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: msg,
		}
	}

	details := make([]string, len(stalled))
	for i, s := range stalled {
		details[i] = fmt.Sprintf("STALLED: %s/%s — branch %s has %d unpushed commit(s)",
			s.rigName, s.name, s.branch, s.unpushedCount)
	}

	return &CheckResult{
		Name:   c.Name(),
		Status: StatusWarning,
		Message: fmt.Sprintf("Found %d stalled polecat(s) with unpushed work at risk of loss",
			len(stalled)),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to push stalled branches not yet landed on the default branch",
	}
}

// Fix pushes branches from stalled polecats to the remote.
//
// It re-verifies each branch immediately before pushing rather than trusting
// the snapshot Run took: Run and Fix can run minutes apart, and in that
// window the content can land on the default branch (another polecat's
// rebase-merge or reimplementation) or the bead behind it can close.
// Pushing on a stale verdict would resurrect a deliberately-superseded
// branch — exactly the harm gt-4vbn reported ("Run 'gt doctor --fix'"
// pushing a branch 3751 lines behind main back to origin).
func (c *StalledPolecatCheck) Fix(ctx *CheckContext) error {
	if len(c.stalledPolecats) == 0 {
		return nil
	}

	var lastErr error
	for _, s := range c.stalledPolecats {
		polecatGit := git.NewGit(s.clonePath)
		_ = polecatGit.FetchDefaultBranchWithTimeout("origin", fetchTimeout) // best-effort, bounded refresh before re-verifying, see Run

		targetStatus, checkErr := polecatGit.BranchTargetStatus(s.branch, "origin", nil)
		if checkErr != nil {
			lastErr = fmt.Errorf("re-checking %s/%s branch %s before push: %w", s.rigName, s.name, s.branch, checkErr)
			continue
		}
		if targetStatus.Preserved {
			continue // Landed since Run — do not resurrect it
		}
		if c.branchSupersededByClosedBead(s.clonePath, s.branch) {
			continue // Bead closed since Run — do not resurrect it
		}

		if err := polecatGit.Push("origin", s.branch, false); err != nil {
			lastErr = fmt.Errorf("pushing %s/%s branch %s: %w", s.rigName, s.name, s.branch, err)
		}
	}
	return lastErr
}

// branchSupersededByClosedBead reports whether the bead encoded in a polecat
// branch name is closed or tombstoned. A terminal bead means the work item
// this branch exists to satisfy is already resolved — by this branch landing
// under a different commit, by a reimplementation elsewhere, or by the issue
// being abandoned — so the branch is not "at risk of loss" even when its
// content cannot be matched against the default branch by git alone.
//
// Returns false (not superseded) whenever this can't be determined: no issue
// encoded in the branch name, or the bead lookup fails (e.g. Dolt down).
// This check only ever narrows an already-flagged branch back out of the
// warning; it never widens the trigger population.
func (c *StalledPolecatCheck) branchSupersededByClosedBead(clonePath, branch string) bool {
	if c.beadStatus == nil {
		return false
	}
	meta, ok := polecat.ParseBranchName(branch)
	if !ok || meta.Issue == "" {
		return false
	}
	status, ok := c.beadStatus(clonePath, meta.Issue)
	if !ok || status == "" {
		return false
	}
	return beads.IssueStatus(status).IsTerminal()
}

// lookupBeadStatus shells out to `bd show <id> --json` to read a bead's
// current status. Best-effort: any failure (bd unavailable, Dolt down,
// unparseable output) reports ok=false so callers fall back to treating the
// bead as unknown rather than blocking on it.
func lookupBeadStatus(workDir, beadID string) (string, bool) {
	if beadID == "" {
		return "", false
	}
	cmd := exec.Command("bd", "show", beadID, "--json") //nolint:gosec // G204: beadID is parsed from a polecat branch name, not external input
	cmd.Dir = workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", false
	}
	var issues []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &issues); err != nil || len(issues) == 0 {
		return "", false
	}
	return issues[0].Status, true
}

// findRigs returns the list of rig names to check.
func (c *StalledPolecatCheck) findRigs(ctx *CheckContext) []string {
	if ctx.RigName != "" {
		return []string{ctx.RigName}
	}

	// Scan town root for rig directories (directories containing polecats/)
	entries, err := os.ReadDir(ctx.TownRoot)
	if err != nil {
		return nil
	}

	var rigs []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == "mayor" {
			continue
		}
		polecatsDir := filepath.Join(ctx.TownRoot, entry.Name(), "polecats")
		if info, err := os.Stat(polecatsDir); err == nil && info.IsDir() {
			rigs = append(rigs, entry.Name())
		}
	}
	return rigs
}

// resolveClonePath finds the worktree path for a polecat.
// Handles both new (polecats/<name>/<rigname>/) and old (polecats/<name>/) structures.
func (c *StalledPolecatCheck) resolveClonePath(townRoot, rigName, polecatName string) string {
	// New structure: polecats/<name>/<rigname>/
	newPath := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if info, err := os.Stat(newPath); err == nil && info.IsDir() {
		return newPath
	}

	// Old structure: polecats/<name>/
	oldPath := filepath.Join(townRoot, rigName, "polecats", polecatName)
	if info, err := os.Stat(filepath.Join(oldPath, ".git")); err == nil && !info.IsDir() {
		return oldPath
	}

	return ""
}

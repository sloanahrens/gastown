package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// fetchTimeout bounds the default-branch refresh below, so an unreachable
// remote cannot hang the scan (gt-ftt).
const fetchTimeout = 30 * time.Second

// beadStatusTimeout bounds the bead lookup below, so an unresponsive Dolt
// costs one bounded wait per candidate rather than stalling the scan.
const beadStatusTimeout = 15 * time.Second

// StalledPolecatCheck detects polecats whose tmux sessions have died while
// their worktrees hold commits that exist on no remote.
//
// The trigger — dead session, branch absent from origin — is the population
// that has always been right; the discriminator is what was missing (gt-4vbn).
// Work already on the default branch under any SHA is confirmed superseded.
// A terminal bead behind the branch is a weaker signal: closing an issue (e.g.
// a "no-changes" reclose after a zombie reset re-dispatches it) proves nothing
// about whether THIS branch's content survives anywhere, so it only stands
// down the auto-push, not the report (gt-5wse). A branch that IS on origin is
// never in scope: origin is already custody.
type StalledPolecatCheck struct {
	FixableCheck
	stalledPolecats []stalledPolecatInfo // Cached during Run for use in Fix

	sessionCheckerForTest polecatSessionChecker             // nil → real tmux
	gitForTest            func(clonePath string) polecatGit // nil → real git.NewGit
	beadStatus            func(townRoot, beadID string) (string, bool)
}

// polecatSessionChecker abstracts the tmux liveness check this check needs,
// so tests can inject a failure without a real tmux server.
type polecatSessionChecker interface {
	HasSession(name string) (bool, error)
}

// polecatGit abstracts the git operations this check needs, so tests can
// inject a failure without a real git repo in a bad state. Push is part of the
// interface because Fix's decision NOT to push is the behavior under test.
type polecatGit interface {
	CurrentBranch() (string, error)
	BranchPushedToRemote(localBranch, remote string) (bool, int, error)
	RemoteDefaultBranch() string
	FetchDefaultBranchWithTimeout(remote string, timeout time.Duration) error
	BranchTargetStatus(localBranch, remote string, targets []string) (git.BranchPreservationStatus, error)
	Push(remote, branch string, force bool) error
}

// gitFor returns the git accessor for a clone path, honoring the test override.
// Run and Fix must resolve it the same way: Fix re-judges what Run judged, so a
// test override that applied to one and not the other would test nothing.
func (c *StalledPolecatCheck) gitFor(clonePath string) polecatGit {
	if c.gitForTest != nil {
		return c.gitForTest(clonePath)
	}
	return git.NewGit(clonePath)
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
	var sessionChecker polecatSessionChecker = tmux.NewTmux()
	if c.sessionCheckerForTest != nil {
		sessionChecker = c.sessionCheckerForTest
	}

	var stalled []stalledPolecatInfo
	var unknown []string     // polecats we could not verify one way or the other
	var needsReview []string // closed bead, but content not confirmed on the default branch — not pushed, but not silently cleared either
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
			id := rigName + "/" + polecatName

			// Check if tmux session is alive
			sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
			alive, err := sessionChecker.HasSession(sessionName)
			if err != nil {
				unknown = append(unknown, fmt.Sprintf("%s: could not check session liveness: %v", id, err))
				continue
			}
			if alive {
				continue // Confirmed alive — not stalled
			}

			// Session is dead. Check for unpushed commits.
			clonePath := c.resolveClonePath(ctx.TownRoot, rigName, polecatName)
			if clonePath == "" {
				continue
			}

			pg := c.gitFor(clonePath)
			branch, brErr := pg.CurrentBranch()
			if brErr != nil {
				unknown = append(unknown, fmt.Sprintf("%s: could not determine current branch: %v", id, brErr))
				continue
			}
			if branch == "" {
				continue
			}

			pushed, unpushedCount, checkErr := pg.BranchPushedToRemote(branch, "origin")
			if checkErr != nil {
				unknown = append(unknown, fmt.Sprintf("%s: could not check push status of branch %s: %v", id, branch, checkErr))
				continue
			}
			if pushed || unpushedCount == 0 {
				continue // Confirmed pushed or nothing ahead — not at risk
			}

			// The branch exists on no remote. Before warning, separate "nobody
			// has this work" from "someone already did it" (gt-4vbn). An
			// inconclusive answer here leaves the branch flagged: the trigger
			// population is right, so uncertainty must not silence it.
			//
			// Content confirmed on the default branch is checked first because
			// it is the strong, independent signal: it proves the same content
			// exists in custody regardless of what anyone did to the bead.
			if c.branchLandedOnDefault(pg, branch) {
				continue
			}
			// A closed/tombstoned bead is only a weak, indirect signal — the
			// issue tracker resolving says nothing about whether THIS branch's
			// specific commits made it anywhere. Trusting it as proof silently
			// discarded genuinely-unique work behind an unrelated closure
			// (gt-5wse). It still stands down the auto-push below (pushing a
			// truly-superseded branch back to origin was gt-4vbn's harm), but
			// it must not make the branch disappear from the report too.
			if c.branchSupersededByTerminalBead(ctx.TownRoot, branch) {
				issue := "unknown-issue"
				if meta, ok := polecat.ParseBranchName(branch); ok && meta.Issue != "" {
					issue = meta.Issue
				}
				needsReview = append(needsReview, fmt.Sprintf(
					"CLOSED-BUT-UNVERIFIED: %s/%s — branch %s has %d commit(s) on no remote behind closed bead %s; content not confirmed on the default branch, review before deleting",
					rigName, polecatName, branch, unpushedCount, issue))
				continue
			}

			stalled = append(stalled, stalledPolecatInfo{
				name:          polecatName,
				rigName:       rigName,
				branch:        branch,
				unpushedCount: unpushedCount,
				clonePath:     clonePath,
			})
		}
	}

	c.stalledPolecats = stalled

	if len(stalled) > 0 {
		details := make([]string, len(stalled))
		for i, s := range stalled {
			details[i] = fmt.Sprintf("STALLED: %s/%s — branch %s has %d commit(s) on no remote",
				s.rigName, s.name, s.branch, s.unpushedCount)
		}
		if len(needsReview) > 0 || len(unknown) > 0 {
			details = append(details, "")
			details = append(details, needsReview...)
			details = append(details, unknown...)
		}

		return &CheckResult{
			Name:   c.Name(),
			Status: StatusWarning,
			Message: fmt.Sprintf("Found %d stalled polecat(s) with unpushed work at risk of loss",
				len(stalled)),
			Details: details,
			FixHint: "Run 'gt doctor --fix' to push stalled branches to remote",
		}
	}

	if len(needsReview) > 0 || len(unknown) > 0 {
		return &CheckResult{
			Name:   c.Name(),
			Status: StatusSkipped,
			Message: fmt.Sprintf("unknown: %d of %d polecat(s) need manual review before their branch is discarded",
				len(needsReview)+len(unknown), checked),
			Details: append(needsReview, unknown...),
		}
	}

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

// branchLandedOnDefault reports whether the branch's content is already on the
// remote default branch, which makes it superseded rather than lost whichever
// SHA carried it (gt-4vbn).
//
// The refresh is load-bearing: a dead polecat's clone stops fetching, so the
// content it is being judged against can postdate its last fetch. Without the
// refresh the check would keep reporting landed work as unlanded forever —
// the failure it exists to remove.
func (c *StalledPolecatCheck) branchLandedOnDefault(pg polecatGit, branch string) bool {
	defaultBranch := pg.RemoteDefaultBranch()
	if defaultBranch == "" {
		return false
	}
	_ = pg.FetchDefaultBranchWithTimeout("origin", fetchTimeout) // best-effort: stale evidence over-flags, it cannot mask real risk

	status, err := pg.BranchTargetStatus(branch, "origin", []string{"origin/" + defaultBranch})
	return err == nil && status.Preserved
}

// branchSupersededByTerminalBead reports whether the bead encoded in the branch
// name is closed or tombstoned. A terminal bead means the work ITEM is
// resolved somehow, but that is not proof this branch's specific content is
// preserved anywhere: a "no-changes" reclose after a zombie reset, a duplicate
// pointing at a branch that never lands, or any other closure unrelated to
// this content, all read as terminal too (gt-5wse). So this signal is only
// strong enough to withhold the auto-push — never to clear the report, which
// stays visible via the needsReview bucket in Run.
//
// False on any uncertainty: no issue in the branch name, or a failed lookup.
func (c *StalledPolecatCheck) branchSupersededByTerminalBead(townRoot, branch string) bool {
	if c.beadStatus == nil {
		return false
	}
	meta, ok := polecat.ParseBranchName(branch)
	if !ok || meta.Issue == "" {
		return false
	}
	status, ok := c.beadStatus(townRoot, meta.Issue)
	if !ok || status == "" {
		return false
	}
	return beads.IssueStatus(status).IsTerminal()
}

// Fix pushes branches from stalled polecats to the remote.
//
// Each branch is re-verified immediately before its push rather than trusting
// Run's snapshot: Run and Fix can run minutes apart, and a branch superseded in
// that window must not be pushed back to origin. Pushing one is the active harm
// gt-4vbn reported — Fix() is reachable with every polecat session dead, so
// nothing else will notice the resurrection.
func (c *StalledPolecatCheck) Fix(ctx *CheckContext) error {
	if len(c.stalledPolecats) == 0 {
		return nil
	}

	var lastErr error
	for _, s := range c.stalledPolecats {
		g := c.gitFor(s.clonePath)
		if c.branchSupersededByTerminalBead(ctx.TownRoot, s.branch) || c.branchLandedOnDefault(g, s.branch) {
			continue
		}
		if err := g.Push("origin", s.branch, false); err != nil {
			lastErr = fmt.Errorf("pushing %s/%s branch %s: %w", s.rigName, s.name, s.branch, err)
		}
	}
	return lastErr
}

// lookupBeadStatus reads a bead's status through bd's prefix routing, so a
// bead living in another rig's database resolves. Bounded, because the caller
// is a doctor scan that must finish. Any failure reports ok=false, which
// callers treat as unknown rather than as an answer.
func lookupBeadStatus(townRoot, beadID string) (string, bool) {
	if townRoot == "" || beadID == "" {
		return "", false
	}
	beadsDir := beads.ResolveBeadsDir(townRoot)
	if _, err := os.Stat(beadsDir); err != nil {
		return "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), beadStatusTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, townRoot, beadsDir, beads.ReadOnlyRouting, "show", beadID, "--json")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	var issues []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &issues); err != nil || len(issues) == 0 {
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

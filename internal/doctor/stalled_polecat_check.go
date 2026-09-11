package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// StalledPolecatCheck detects polecats whose tmux sessions have died but whose
// worktrees still contain unpushed commits. These are the most dangerous failure
// mode after disk space exhaustion: the polecat appears dead, and nuking it
// would permanently lose the committed work on its branch.
//
// This check warns about at-risk branches so they can be pushed before cleanup.
type StalledPolecatCheck struct {
	FixableCheck
	stalledPolecats []stalledPolecatInfo // Cached during Run for use in Fix

	sessionCheckerForTest polecatSessionChecker             // nil → real tmux
	gitForTest            func(clonePath string) polecatGit // nil → real git.NewGit
}

// polecatSessionChecker abstracts the tmux liveness check this check needs,
// so tests can inject a failure without a real tmux server.
type polecatSessionChecker interface {
	HasSession(name string) (bool, error)
}

// polecatGit abstracts the git operations this check needs, so tests can
// inject a failure without a real git repo in a bad state.
type polecatGit interface {
	CurrentBranch() (string, error)
	BranchPushedToRemote(localBranch, remote string) (bool, int, error)
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
	}
}

// Run checks all rigs for polecats with dead sessions and unpushed commits.
func (c *StalledPolecatCheck) Run(ctx *CheckContext) *CheckResult {
	var sessionChecker polecatSessionChecker = tmux.NewTmux()
	if c.sessionCheckerForTest != nil {
		sessionChecker = c.sessionCheckerForTest
	}
	newGit := func(clonePath string) polecatGit { return git.NewGit(clonePath) }
	if c.gitForTest != nil {
		newGit = c.gitForTest
	}

	var stalled []stalledPolecatInfo
	var unknown []string // polecats we could not verify one way or the other
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

			pg := newGit(clonePath)
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
			if pushed {
				continue // Confirmed pushed — not at risk
			}

			if unpushedCount > 0 {
				stalled = append(stalled, stalledPolecatInfo{
					name:          polecatName,
					rigName:       rigName,
					branch:        branch,
					unpushedCount: unpushedCount,
					clonePath:     clonePath,
				})
			}
		}
	}

	c.stalledPolecats = stalled

	if len(stalled) > 0 {
		details := make([]string, len(stalled))
		for i, s := range stalled {
			details[i] = fmt.Sprintf("STALLED: %s/%s — branch %s has %d unpushed commit(s)",
				s.rigName, s.name, s.branch, s.unpushedCount)
		}
		if len(unknown) > 0 {
			details = append(details, "")
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

	if len(unknown) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: fmt.Sprintf("unknown: could not check %d of %d polecat(s) for stalled work", len(unknown), checked),
			Details: unknown,
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

// Fix pushes branches from stalled polecats to the remote.
func (c *StalledPolecatCheck) Fix(ctx *CheckContext) error {
	if len(c.stalledPolecats) == 0 {
		return nil
	}

	var lastErr error
	for _, s := range c.stalledPolecats {
		g := git.NewGit(s.clonePath)
		if err := g.Push("origin", s.branch, false); err != nil {
			lastErr = fmt.Errorf("pushing %s/%s branch %s: %w", s.rigName, s.name, s.branch, err)
		}
	}
	return lastErr
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

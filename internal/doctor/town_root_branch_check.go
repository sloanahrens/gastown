package doctor

import (
	"fmt"
)

// TownRootBranchCheck verifies that the town root directory is on the main branch.
// The town root should always stay on main to avoid confusion and broken gt commands.
// Accidental branch switches can happen when git commands run in the wrong directory.
type TownRootBranchCheck struct {
	FixableCheck
	currentBranch string // Cached during Run for use in Fix
}

// NewTownRootBranchCheck creates a new town root branch check.
func NewTownRootBranchCheck() *TownRootBranchCheck {
	return &TownRootBranchCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "town-root-branch",
				CheckDescription: "Verify town root is on main branch",
				CheckCategory:    CategoryCore,
			},
		},
	}
}

// Run checks if the town root is on the main branch.
func (c *TownRootBranchCheck) Run(ctx *CheckContext) *CheckResult {
	// Get current branch
	branch, err := ctx.git(ctx.TownRoot).CurrentBranch()
	if err != nil {
		// Could not determine the current branch (not a git repo, or some
		// other git failure) — we did not verify the branch is main.
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not determine town root's current branch",
			Details: []string{err.Error()},
		}
	}

	if branch == "HEAD" {
		branch = "" // detached
	}
	c.currentBranch = branch

	// Empty branch means detached HEAD
	if branch == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Town root is in detached HEAD state",
			Details: []string{
				"The town root should be on the main branch",
				"Detached HEAD can cause gt commands to fail",
			},
			FixHint: "Run 'gt doctor --fix' or manually: cd ~/gt && git checkout main",
		}
	}

	// Accept main or master
	if branch == "main" || branch == "master" || branch == "gt_managed" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("Town root is on %s branch", branch),
		}
	}

	// On wrong branch - this is the problem we're trying to prevent
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("Town root is on wrong branch: %s", branch),
		Details: []string{
			"The town root (~/gt) must stay on main branch",
			fmt.Sprintf("Currently on: %s", branch),
			"This can cause gt commands to fail (missing rigs.json, etc.)",
			"The branch switch was likely accidental (git command in wrong dir)",
		},
		FixHint: "Run 'gt doctor --fix' or manually: cd ~/gt && git checkout main",
	}
}

// Fix switches the town root back to main branch.
func (c *TownRootBranchCheck) Fix(ctx *CheckContext) error {
	// Only fix if we're not already on main
	if c.currentBranch == "main" || c.currentBranch == "master" || c.currentBranch == "gt_managed" {
		return nil
	}

	// Check for uncommitted changes that would block checkout
	g := ctx.git(ctx.TownRoot)
	status, err := g.Status()
	if err != nil {
		return fmt.Errorf("failed to check git status: %w", err)
	}

	if !status.Clean {
		return fmt.Errorf("cannot switch to main: uncommitted changes in town root (stash or commit first)")
	}

	// Switch to main
	if err := g.Checkout("main"); err != nil {
		// Try master if main doesn't exist
		if err := g.Checkout("master"); err != nil {
			return fmt.Errorf("failed to checkout main: %w", err)
		}
	}

	return nil
}

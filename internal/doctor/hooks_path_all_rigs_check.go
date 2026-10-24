package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HooksPathAllRigsCheck verifies all clones across all rigs have core.hooksPath set.
// This runs globally (without --rig) to ensure the pre-push hook is active everywhere.
// The pre-push hook enforces integration branch landing guardrails.
type HooksPathAllRigsCheck struct {
	FixableCheck
	unconfiguredClones []string
}

// NewHooksPathAllRigsCheck creates a new global hooks path check.
func NewHooksPathAllRigsCheck() *HooksPathAllRigsCheck {
	return &HooksPathAllRigsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "hooks-path-all-rigs",
				CheckDescription: "Check core.hooksPath is set for all clones across all rigs",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks all clones in all rigs for core.hooksPath configuration.
func (c *HooksPathAllRigsCheck) Run(ctx *CheckContext) *CheckResult {
	rigs := findAllRigs(ctx.TownRoot)
	if len(rigs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs found",
		}
	}

	c.unconfiguredClones = nil
	totalClones := 0

	for _, rigPath := range rigs {
		clonePaths := findRigClones(rigPath)
		for _, clonePath := range clonePaths {
			// Skip if no .githooks directory (repo doesn't use hooks)
			if _, err := os.Stat(filepath.Join(clonePath, ".githooks")); os.IsNotExist(err) {
				continue
			}
			totalClones++

			hooksPath, err := ctx.git(clonePath).ConfigGet("core.hooksPath")
			if err != nil || strings.TrimSpace(hooksPath) != ".githooks" {
				c.unconfiguredClones = append(c.unconfiguredClones, clonePath)
			}
		}
	}

	if len(c.unconfiguredClones) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d clone(s) have hooks configured", totalClones),
		}
	}

	var details []string
	for _, clonePath := range c.unconfiguredClones {
		relPath, _ := filepath.Rel(ctx.TownRoot, clonePath)
		if relPath == "" {
			relPath = clonePath
		}
		details = append(details, relPath)
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d clone(s) missing core.hooksPath across all rigs", len(c.unconfiguredClones)),
		Details: details,
		FixHint: "Run 'gt doctor fix hooks-path-all-rigs' to configure hooks",
	}
}

// Fix configures core.hooksPath for all unconfigured clones.
func (c *HooksPathAllRigsCheck) Fix(ctx *CheckContext) error {
	for _, clonePath := range c.unconfiguredClones {
		if err := ctx.git(clonePath).ConfigSet("core.hooksPath", ".githooks"); err != nil {
			return fmt.Errorf("failed to configure hooks for %s: %w", clonePath, err)
		}
	}
	return nil
}

// findRigClones returns all git clone paths within a rig. The rig is named by
// its directory, as in every town layout.
func findRigClones(rigPath string) []string {
	return rigCloneDirs(rigPath, filepath.Base(rigPath))
}

// rigCloneDirs returns the git clones of a rig that carry core.hooksPath: the
// mayor clone, the crew clones and the polecat clones. HooksPathConfiguredCheck
// and HooksPathAllRigsCheck both enumerate through it so they judge the same
// set. A crew member is a clone, not a linked worktree (see crewCloneDirs). A
// polecat worktree is nested at polecats/<name>/<rigName>/; the flat
// polecats/<name>/ layout predates that nesting. Only a nested .git proves the
// worktree, so a failed stat for any reason falls back to the flat path, which
// the .git filter below judges.
func rigCloneDirs(rigPath, rigName string) []string {
	clones := []string{filepath.Join(rigPath, "mayor", "rig")}
	clones = append(clones, crewCloneDirs(filepath.Join(rigPath, "crew"))...)

	polecatDir := filepath.Join(rigPath, "polecats")
	if entries, err := os.ReadDir(polecatDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			clonePath := filepath.Join(polecatDir, entry.Name(), rigName)
			if _, err := os.Stat(filepath.Join(clonePath, ".git")); err != nil {
				clonePath = filepath.Join(polecatDir, entry.Name())
			}
			clones = append(clones, clonePath)
		}
	}

	// Filter to only existing git repos
	var valid []string
	for _, clonePath := range clones {
		if _, err := os.Stat(filepath.Join(clonePath, ".git")); err == nil {
			valid = append(valid, clonePath)
		}
	}
	return valid
}

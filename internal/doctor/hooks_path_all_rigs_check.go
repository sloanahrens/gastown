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

// findRigClones returns all git clone paths within a rig.
func findRigClones(rigPath string) []string {
	return rigCloneDirs(rigPath)
}

// rigCloneDirs returns the git clones of a rig: the mayor clone, the crew clones
// and the polecat clones. HooksPathConfiguredCheck and HooksPathAllRigsCheck
// both enumerate through it so they judge the same set. Linked crew worktrees
// are left out (see crewCloneDirs). Only paths with a .git are returned.
func rigCloneDirs(rigPath string) []string {
	clones := []string{filepath.Join(rigPath, "mayor", "rig")}
	clones = append(clones, crewCloneDirs(filepath.Join(rigPath, "crew"))...)

	polecatDir := filepath.Join(rigPath, "polecats")
	if entries, err := os.ReadDir(polecatDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				clones = append(clones, polecatCloneDirs(filepath.Join(polecatDir, entry.Name()))...)
			}
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

// polecatCloneDirs returns the clones of one polecat. A worktree is nested at
// polecats/<name>/<dir>/, where <dir> is the rig's directory in every town
// layout, but any subdirectory holding a clone is taken so that one nested under
// another name is still judged. The flat polecats/<name>/ layout predates that
// nesting, so it is the clone only when no nested clone exists.
func polecatCloneDirs(polecat string) []string {
	if entries, err := os.ReadDir(polecat); err == nil {
		var nested []string
		for _, entry := range entries {
			clonePath := filepath.Join(polecat, entry.Name())
			if entry.IsDir() && isCloneDir(clonePath) {
				nested = append(nested, clonePath)
			}
		}
		if len(nested) > 0 {
			return nested
		}
	}
	if isCloneDir(polecat) {
		return []string{polecat}
	}
	return nil
}

// isCloneDir reports whether dir holds a clone, a .git directory. A linked
// worktree's .git is a file, and crewCloneDirs leaves those out as well.
func isCloneDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}

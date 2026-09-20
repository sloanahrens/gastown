// ABOUTME: Doctor check for unused directive files.
// ABOUTME: Detects directive files that don't correspond to known agent roles.

package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// UnusedDirectiveCheck detects directive files that don't correspond to known
// agent roles. These files are silently never loaded by Gas Town.
type UnusedDirectiveCheck struct {
	FixableCheck
	// Cached unused directive info for use in Fix
	unusedFiles []string
}

// NewUnusedDirectiveCheck creates a new check for unused directive files.
func NewUnusedDirectiveCheck() *UnusedDirectiveCheck {
	return &UnusedDirectiveCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "unused-directives",
				CheckDescription: "Detect directive files not named for a known agent role",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks for directive files that don't match known agent roles.
func (c *UnusedDirectiveCheck) Run(ctx *CheckContext) *CheckResult {
	// Build a map of valid roles for quick lookup
	validRoles := make(map[string]bool)
	for _, r := range config.AllRoles() {
		validRoles[r] = true
	}

	var unusedFiles []string
	var unusedRoles []string

	// Check town-level directives
	townDir := filepath.Join(ctx.TownRoot, "directives")
	if files, err := os.ReadDir(townDir); err == nil {
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
				continue
			}
			role := strings.TrimSuffix(f.Name(), ".md")
			if !validRoles[role] {
				path := filepath.Join(townDir, f.Name())
				unusedFiles = append(unusedFiles, path)
				unusedRoles = append(unusedRoles, role)
			}
		}
	}

	// Check rig-level directives if a specific rig is being checked
	// or scan all rigs
	if ctx.RigName != "" {
		c.checkRigDirectives(ctx.TownRoot, ctx.RigName, validRoles, &unusedFiles, &unusedRoles)
	} else {
		// Scan all rigs
		rigDirs, err := os.ReadDir(ctx.TownRoot)
		if err == nil {
			for _, d := range rigDirs {
				if !d.IsDir() {
					continue
				}
				rigName := d.Name()
				rigConfigPath := filepath.Join(ctx.TownRoot, rigName, "config.json")
				if _, err := os.Stat(rigConfigPath); err != nil {
					continue
				}
				c.checkRigDirectives(ctx.TownRoot, rigName, validRoles, &unusedFiles, &unusedRoles)
			}
		}
	}

	// Cache for Fix
	c.unusedFiles = unusedFiles

	if len(unusedFiles) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No unused directive files found",
		}
	}

	details := make([]string, len(unusedFiles))
	for i, f := range unusedFiles {
		details[i] = fmt.Sprintf("Unused: %s", f)
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d unused directive file(s) detected", len(unusedFiles)),
		Details: details,
		FixHint: "Review and remove or rename unused directive files",
	}
}

// checkRigDirectives checks rig-level directives for unused files.
func (c *UnusedDirectiveCheck) checkRigDirectives(townRoot, rigName string, validRoles map[string]bool, unusedFiles *[]string, unusedRoles *[]string) {
	rigDir := filepath.Join(townRoot, rigName, "directives")
	files, err := os.ReadDir(rigDir)
	if err != nil {
		return
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
			continue
		}
		role := strings.TrimSuffix(f.Name(), ".md")
		if !validRoles[role] {
			path := filepath.Join(rigDir, f.Name())
			*unusedFiles = append(*unusedFiles, path)
			*unusedRoles = append(*unusedRoles, role)
		}
	}
}

// Fix removes unused directive files.
func (c *UnusedDirectiveCheck) Fix(ctx *CheckContext) error {
	for _, path := range c.unusedFiles {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("failed to remove %s: %w", path, err)
		}
	}
	return nil
}

package doctor

import (
	"fmt"
	"os"
	"path/filepath"
)

// OrphanedRoleOverridesCheck reports role override files that no runtime path
// reads. Role definitions come from the binary's embedded defaults, so a
// <town>/roles/*.toml or <rig>/roles/*.toml file has no effect; the check
// names those files so they can be deleted instead of silently lingering.
type OrphanedRoleOverridesCheck struct {
	BaseCheck
}

// NewOrphanedRoleOverridesCheck creates a new orphaned role override check.
func NewOrphanedRoleOverridesCheck() *OrphanedRoleOverridesCheck {
	return &OrphanedRoleOverridesCheck{
		BaseCheck: BaseCheck{
			CheckName:        "orphaned-role-overrides",
			CheckDescription: "Report role override files that no runtime path reads",
			CheckCategory:    CategoryConfig,
		},
	}
}

// Run lists the role override files under the town and under each rig.
func (c *OrphanedRoleOverridesCheck) Run(ctx *CheckContext) *CheckResult {
	var orphans []string

	orphans = append(orphans, roleOverrideFiles(filepath.Join(ctx.TownRoot, "roles"))...)

	// Discover rigs by looking for directories with rig.json
	if entries, err := os.ReadDir(ctx.TownRoot); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			rigName := entry.Name()
			if _, err := os.Stat(filepath.Join(ctx.TownRoot, rigName, "rig.json")); err != nil {
				continue
			}
			orphans = append(orphans, roleOverrideFiles(filepath.Join(ctx.TownRoot, rigName, "roles"))...)
		}
	}

	if len(orphans) > 0 {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusWarning,
			Message:  fmt.Sprintf("%d role override file(s) are not read by any runtime path", len(orphans)),
			Details:  orphans,
			FixHint:  "Delete these files; role definitions are built into the gt binary",
			Category: c.Category(),
		}
	}

	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusOK,
		Message:  "No orphaned role override files",
		Category: c.Category(),
	}
}

// roleOverrideFiles returns the *.toml files directly inside dir, or nil if
// dir cannot be read.
func roleOverrideFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".toml" {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	return files
}

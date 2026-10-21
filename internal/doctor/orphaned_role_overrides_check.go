package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// OrphanedRoleOverridesCheck reports role override files that no runtime path
// reads, so an operator can delete them instead of leaving dead config in the
// town. Role definitions come from the binary's embedded defaults
// (config.loadBuiltinRoleDefinition); the loader that read <town>/roles and
// <rig>/roles was retired in gt-7mthw. TestRoleOverridesHaveNoReader pins that
// premise: a reader reintroduced without narrowing this check would make
// doctor tell users to delete live config.
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

// Run lists the role override files under the town and under each registered
// rig.
func (c *OrphanedRoleOverridesCheck) Run(ctx *CheckContext) *CheckResult {
	// A registry that cannot be read leaves the rig-level overrides unknown;
	// rigs-json owns reporting why, and this check must not read the gap as
	// clean.
	rigs, err := discoverRigs(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusSkipped,
			Message:  fmt.Sprintf("Could not read the rig registry: %v", err),
			Category: c.Category(),
		}
	}

	dirs := []string{filepath.Join(ctx.TownRoot, "roles")}
	for _, rig := range rigs {
		dirs = append(dirs, filepath.Join(ctx.TownRoot, rig, "roles"))
	}
	sort.Strings(dirs)

	var orphans, unreadable []string
	for _, dir := range dirs {
		files, err := roleOverrideFiles(dir)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s: %v", dir, err))
			continue
		}
		orphans = append(orphans, files...)
	}
	sort.Strings(orphans)

	if len(unreadable) > 0 {
		// An unreadable dir hides orphans rather than proving there are none,
		// so the result is inconclusive even when other dirs were clean.
		message := fmt.Sprintf("%d role override path(s) could not be read", len(unreadable))
		if len(orphans) > 0 {
			message += fmt.Sprintf("; %d orphaned file(s) found in the directories that could be read", len(orphans))
		}
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusSkipped,
			Message:  message,
			Details:  append(append([]string{}, unreadable...), orphans...),
			FixHint:  "Make these directories readable, then re-run gt doctor",
			Category: c.Category(),
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

// roleOverrideFiles returns the .toml files directly inside dir. A missing dir
// yields none; any other read error is returned so the caller can report the
// answer as unknown instead of empty.
func roleOverrideFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".toml" {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	return files, nil
}

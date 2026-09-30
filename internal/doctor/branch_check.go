package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CloneDivergenceCheck detects when git clones have drifted significantly apart.
// This is an emergency condition - all clones should be tracking origin/main
// and staying reasonably in sync. Divergence here is different from beads-sync
// divergence, which is expected.
type CloneDivergenceCheck struct {
	BaseCheck
}

// NewCloneDivergenceCheck creates a new clone divergence check.
func NewCloneDivergenceCheck() *CloneDivergenceCheck {
	return &CloneDivergenceCheck{
		BaseCheck: BaseCheck{
			CheckName:        "clone-divergence",
			CheckDescription: "Detect emergency divergence between git clones",
			CheckCategory:    CategoryCleanup,
		},
	}
}

// cloneInfo holds information about a single clone.
type cloneInfo struct {
	path     string
	branch   string
	headSHA  string
	behindBy int // commits behind origin/main
}

// Run checks for significant divergence between clones.
func (c *CloneDivergenceCheck) Run(ctx *CheckContext) *CheckResult {
	clones := c.findAllClones(ctx.TownRoot)
	if len(clones) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No clones found",
		}
	}

	// Gather info about each clone
	var infos []cloneInfo
	for _, path := range clones {
		info, err := c.getCloneInfo(path)
		if err != nil {
			continue // Skip problematic clones
		}
		infos = append(infos, info)
	}

	if len(infos) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No valid git clones found",
		}
	}

	// Check for clones significantly behind origin/main
	var warnings []string
	var errors []string

	for _, info := range infos {
		relPath := c.relativePath(ctx.TownRoot, info.path)

		// Only check clones on main branch (others are caught by BranchCheck)
		if info.branch != "main" && info.branch != "master" {
			continue
		}

		if info.behindBy > 50 {
			errors = append(errors, fmt.Sprintf("%s: %d commits behind origin/main (EMERGENCY)", relPath, info.behindBy))
		} else if info.behindBy > 10 {
			warnings = append(warnings, fmt.Sprintf("%s: %d commits behind origin/main", relPath, info.behindBy))
		}
	}

	if len(errors) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d clone(s) critically diverged", len(errors)),
			Details: append(errors, warnings...),
			FixHint: "Run 'git pull --rebase' in affected directories",
		}
	}

	if len(warnings) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d clone(s) behind origin/main", len(warnings)),
			Details: warnings,
			FixHint: "Run 'git pull --rebase' in affected directories",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("All %d clones in sync with origin/main", len(infos)),
	}
}

// findAllClones finds all git clones in the workspace.
func (c *CloneDivergenceCheck) findAllClones(townRoot string) []string {
	var clones []string

	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return clones
	}

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == "mayor" || entry.Name() == "docs" {
			continue
		}

		rigPath := filepath.Join(townRoot, entry.Name())

		// Check standard clone locations
		locations := []string{
			"mayor/rig",
		}

		for _, loc := range locations {
			path := filepath.Join(rigPath, loc)
			if c.isGitRepo(path) {
				clones = append(clones, path)
			}
		}

		// Add crew members
		crewPath := filepath.Join(rigPath, "crew")
		if crewEntries, err := os.ReadDir(crewPath); err == nil {
			for _, crew := range crewEntries {
				if crew.IsDir() && !strings.HasPrefix(crew.Name(), ".") {
					path := filepath.Join(crewPath, crew.Name())
					if c.isGitRepo(path) {
						clones = append(clones, path)
					}
				}
			}
		}

		// Add polecats (handle both new and old structures)
		// New structure: polecats/<name>/<rigname>/
		// Old structure: polecats/<name>/
		rigName := entry.Name()
		polecatsPath := filepath.Join(rigPath, "polecats")
		if polecatEntries, err := os.ReadDir(polecatsPath); err == nil {
			for _, polecat := range polecatEntries {
				if polecat.IsDir() && !strings.HasPrefix(polecat.Name(), ".") {
					// Try new structure first
					path := filepath.Join(polecatsPath, polecat.Name(), rigName)
					if !c.isGitRepo(path) {
						// Fall back to old structure
						path = filepath.Join(polecatsPath, polecat.Name())
					}
					if c.isGitRepo(path) {
						clones = append(clones, path)
					}
				}
			}
		}
	}

	return clones
}

// isGitRepo checks if a directory is a git repository.
func (c *CloneDivergenceCheck) isGitRepo(path string) bool {
	gitDir := filepath.Join(path, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		return true
	}
	return false
}

// getCloneInfo gathers information about a clone.
func (c *CloneDivergenceCheck) getCloneInfo(path string) (cloneInfo, error) {
	info := cloneInfo{path: path}

	// Get current branch
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = path
	out, err := cmd.Output()
	if err != nil {
		return info, err
	}
	info.branch = strings.TrimSpace(string(out))

	// Get HEAD SHA
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = path
	out, err = cmd.Output()
	if err != nil {
		return info, err
	}
	info.headSHA = strings.TrimSpace(string(out))

	// Count commits behind origin/main (uses existing refs, may be stale)
	cmd = exec.Command("git", "rev-list", "--count", "HEAD..origin/main")
	cmd.Dir = path
	out, err = cmd.Output()
	if err != nil {
		// origin/main might not exist, treat as 0 behind
		info.behindBy = 0
		return info, nil
	}

	var behind int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &behind)
	info.behindBy = behind

	return info, nil
}

// relativePath returns path relative to base.
func (c *CloneDivergenceCheck) relativePath(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return rel
}

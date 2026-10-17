package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/version"
)

// CloneDivergenceCheck detects when git clones have drifted significantly apart.
// This is an emergency condition - all clones should be tracking origin/main
// and staying reasonably in sync. Divergence here is different from beads-sync
// divergence, which is expected. A rig's unmaintained mayor/rig clone is not
// one of them (unmaintainedMayorClone).
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

	liveSource := liveSourceCheckout(ctx)

	// Gather info about each clone
	var infos []cloneInfo
	for _, path := range clones {
		info, err := c.getCloneInfo(ctx, path)
		if err != nil {
			continue // Skip problematic clones
		}
		if c.unmaintainedMayorClone(ctx, info, liveSource) {
			continue
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

// liveSourceCheckout returns the mayor/rig clone the town itself reads — its gt
// source checkout, which the installed binary is built from and which gt plugin
// sync takes the town's plugins out of — so that clone's distance from
// origin/main stays a finding (gt-9gnar): a stale clone there ships stale
// plugins. Every other rig's canonical repo is its shared .repo.git, which
// polecat/manager.go's repoBase prefers to mayor/rig. The path is resolved, not
// spelled out, so the rule is the layout the rest of the town uses rather than
// a rig name (gt-jx8sh). "" means the town has no source checkout, and then no
// mayor/rig clone is live.
func liveSourceCheckout(ctx *CheckContext) string {
	repoRoot, err := version.GetRepoRootForTown(ctx.TownRoot)
	if err != nil {
		return ""
	}
	return repoRoot
}

// unmaintainedMayorClone reports whether info is a mayor/rig clone the town
// stopped working in: a clone that is not liveSource, the town's own gt source
// checkout, and holds no commits of its own, so it is a stale copy of the rig's
// shared .repo.git and nothing else (gt-9gnar). Reporting its distance from
// origin/main buried the real divergence in gt-s5zor. A clone holding its own
// commits is a working copy wherever it sits, so that one keeps the check it
// has today.
func (c *CloneDivergenceCheck) unmaintainedMayorClone(ctx *CheckContext, info cloneInfo, liveSource string) bool {
	rig := mayorCloneRig(ctx.TownRoot, info.path)
	if rig == "" || info.path == liveSource {
		return false
	}
	// HEAD is an ancestor of origin/main exactly when the clone holds no
	// commits of its own.
	base, err := ctx.git(info.path).MergeBase("HEAD", "origin/main")
	return err == nil && base == info.headSHA
}

// mayorCloneRig is the rig whose mayor/rig clone path is, or "" when path is
// one of the rig's other clones (a crew or polecat checkout).
func mayorCloneRig(townRoot, path string) string {
	rel, err := filepath.Rel(townRoot, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[1] != "mayor" || parts[2] != "rig" {
		return ""
	}
	return parts[0]
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
		for _, path := range crewCloneDirs(filepath.Join(rigPath, "crew")) {
			if c.isGitRepo(path) {
				clones = append(clones, path)
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
func (c *CloneDivergenceCheck) getCloneInfo(ctx *CheckContext, path string) (cloneInfo, error) {
	info := cloneInfo{path: path}
	g := ctx.git(path)

	branch, err := currentBranch(g)
	if err != nil {
		return info, err
	}
	info.branch = branch

	if info.headSHA, err = g.Rev("HEAD"); err != nil {
		return info, err
	}

	// Count commits behind origin/main (uses existing refs, may be stale);
	// origin/main might not exist, which counts as 0 behind.
	info.behindBy, _ = g.CountCommitsBehind("origin/main")
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

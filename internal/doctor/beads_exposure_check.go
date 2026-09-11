package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/rig"
)

// BeadsExposureCheck verifies .beads/ is protected from `git clean -fd` in
// every clone across every rig.
//
// Protection can come from either a tracked .gitignore entry OR a per-clone
// .git/info/exclude entry — but info/exclude never propagates across clones
// (it's per-repository and is never committed), so whether a given clone is
// protected depends entirely on how it was provisioned. A clone missing
// this protection shows .beads/ as untracked in `git status`, one
// `git clean -fd` away from deleting the beads credential key, audit log,
// and backups (gt-ylpg).
//
// This check verifies EFFECTIVE ignore status per clone via `git status
// --ignored`, not by scanning .gitignore text — a clone protected only by
// info/exclude has no matching .gitignore line to find, and a stale
// .gitignore line can't prove anything about the clone's actual git state.
type BeadsExposureCheck struct {
	FixableCheck
	exposedClones []string
}

// NewBeadsExposureCheck creates a new .beads/ exposure check.
func NewBeadsExposureCheck() *BeadsExposureCheck {
	return &BeadsExposureCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "beads-exposure",
				CheckDescription: "Check that .beads/ is protected from git clean in every clone",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks every clone in every rig for an untracked, unignored .beads/ directory.
func (c *BeadsExposureCheck) Run(ctx *CheckContext) *CheckResult {
	rigs := findAllRigs(ctx.TownRoot)
	if len(rigs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs found",
		}
	}

	c.exposedClones = nil
	checked := 0

	for _, rigPath := range rigs {
		for _, clonePath := range findRigClones(rigPath) {
			if _, err := os.Stat(filepath.Join(clonePath, ".beads")); err != nil {
				continue // nothing to protect yet in this clone
			}
			checked++
			if beadsUntrackedAndUnignored(clonePath) {
				c.exposedClones = append(c.exposedClones, clonePath)
			}
		}
	}

	if checked == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No .beads/ directories found in any clone",
		}
	}

	if len(c.exposedClones) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf(".beads/ protected in all %d checked clone(s)", checked),
		}
	}

	var details []string
	for _, clonePath := range c.exposedClones {
		relPath, _ := filepath.Rel(ctx.TownRoot, clonePath)
		if relPath == "" {
			relPath = clonePath
		}
		details = append(details, fmt.Sprintf("%s: .beads/ is untracked and not gitignored — `git clean -fd` would delete it", relPath))
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d clone(s) have an unprotected .beads/ directory", len(c.exposedClones)),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to add .beads/ to each clone's local git exclude",
	}
}

// Fix adds .beads/ to the local git exclude file for each exposed clone.
func (c *BeadsExposureCheck) Fix(ctx *CheckContext) error {
	for _, clonePath := range c.exposedClones {
		if err := rig.EnsureLocalExcludePatterns(clonePath); err != nil {
			return fmt.Errorf("protecting .beads/ in %s: %w", clonePath, err)
		}
	}
	return nil
}

// beadsUntrackedAndUnignored reports whether .beads/ in clonePath is exposed
// to `git clean -fd`: present, and appearing as untracked ("??") in git
// status. Scoping the status query to the .beads pathspec means every "??"
// line in the output — whether the whole directory or individual files
// inside it — is a real exposure; a tracked or ignored .beads/ produces no
// such lines regardless of which mechanism (tracked exception, .gitignore,
// or info/exclude) is doing the protecting.
func beadsUntrackedAndUnignored(clonePath string) bool {
	cmd := exec.Command("git", "-C", clonePath, "status", "--porcelain", "--ignored", "--", ".beads")
	out, err := cmd.Output()
	if err != nil {
		return false // not a git repo, or git failed — don't flag on uncertain state
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "??") {
			return true
		}
	}
	return false
}

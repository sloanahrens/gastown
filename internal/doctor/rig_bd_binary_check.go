package doctor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// rigBDBinaryScanSubdirs are the rig subdirectories whose immediate children
// are worktrees: crew/<name>, polecats/<name>/<rigname>, refinery/<name>,
// mayor/<name>. The walk never starts higher than one of these children.
var rigBDBinaryScanSubdirs = []string{"crew", "polecats", "refinery", "mayor"}

// rigBDBinaryMaxDepth bounds the walk beneath each worktree candidate: deep
// enough for a build output at a repo root (depth 0) and for the polecat's
// nested <name>/<rigname> level, no deeper.
const rigBDBinaryMaxDepth = 3

// RigBDBinaryCheck reports executable files named bd left inside rig
// worktrees, where a PATH search or a copy can reach the production binary
// (hq-4exu7).
type RigBDBinaryCheck struct {
	BaseCheck
}

// NewRigBDBinaryCheck creates a new rig-bd-binary check.
func NewRigBDBinaryCheck() *RigBDBinaryCheck {
	return &RigBDBinaryCheck{
		BaseCheck: BaseCheck{
			CheckName:        "rig-bd-binary",
			CheckDescription: "Detect executable bd binaries left in rig worktrees",
			CheckCategory:    CategoryCleanup,
		},
	}
}

// Run walks each rig's worktree candidates and reports every executable bd it
// finds. It never changes a file; the remedy is printed per finding.
func (c *RigBDBinaryCheck) Run(ctx *CheckContext) *CheckResult {
	if ctx.TownRoot == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "town root unknown: cannot tell where the rigs are",
		}
	}
	// A town root that cannot be listed would yield no scan roots and a
	// bogus clean pass, so report that it could not be looked at instead.
	if _, err := os.ReadDir(ctx.TownRoot); err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "Cannot read town root",
			Details: []string{err.Error()},
		}
	}

	roots := rigBDBinaryScanRoots(ctx.TownRoot)
	if len(roots) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rig worktrees found",
		}
	}

	var found []string
	for _, root := range roots {
		found = append(found, findExecutableBDIn(root)...)
	}

	if len(found) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No executable bd binaries in rig worktrees",
		}
	}

	details := make([]string, 0, len(found))
	for _, path := range found {
		details = append(details, fmt.Sprintf(
			"%s: executable bd binary — remedy: chmod 644 %s",
			relToTown(ctx, path), path))
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("%d executable bd binary(ies) left in rig worktrees", len(found)),
		Details: details,
		FixHint: "Remove the execute bit from each reported path: chmod 644 <path>",
	}
}

// rigBDBinaryScanRoots returns the worktree candidates to search: the
// immediate children of crew, polecats, refinery and mayor under every rig.
// Only existing rig directories are walked, and only beneath those four
// subdirectories — never the town root, / or HOME.
func rigBDBinaryScanRoots(townRoot string) []string {
	var roots []string
	for _, rigPath := range findAllRigs(townRoot) {
		for _, sub := range rigBDBinaryScanSubdirs {
			entries, err := os.ReadDir(filepath.Join(rigPath, sub))
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
					roots = append(roots, filepath.Join(rigPath, sub, entry.Name()))
				}
			}
		}
	}
	return roots
}

// findExecutableBDIn walks root up to rigBDBinaryMaxDepth and returns every
// regular file named bd with any execute bit set. Directories named bd are
// descended into but are not findings, a mode 644 file is not a finding, and
// symlinks are never followed (WalkDir does not follow them, and a symlink is
// not a regular file).
func findExecutableBDIn(root string) []string {
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))

	var found []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is skipped; the rest of the walk still runs.
			return nil
		}

		if strings.Count(filepath.Clean(path), string(os.PathSeparator))-rootDepth > rigBDBinaryMaxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}

		if d.Name() != "bd" {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		if info.Mode().Perm()&0o111 != 0 {
			found = append(found, path)
		}
		return nil
	})
	return found
}

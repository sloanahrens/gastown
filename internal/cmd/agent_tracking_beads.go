package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
)

// findCwdBeadsWorkDir finds the nearest .beads directory by walking up from CWD.
// It intentionally ignores BEADS_DIR for callers whose target is implied by
// the current rig worktree rather than inherited session environment.
func findCwdBeadsWorkDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return findBeadsWorkDirFrom(cwd)
}

// findBeadsWorkDirFrom finds the nearest directory at or above start that
// holds a .beads directory.
func findBeadsWorkDirFrom(start string) (string, error) {
	path := start
	for {
		if _, err := os.Stat(filepath.Join(path, ".beads")); err == nil {
			return path, nil
		}

		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}

	return "", fmt.Errorf("no .beads directory found")
}

// resolveAgentTrackingBeadsDir resolves the bead database used for agent state.
// Agent tracking follows the agent's current rig, so cwd-local redirects must
// win over an inherited town-level BEADS_DIR. The env-first resolver remains a
// fallback for contexts that do not have a cwd-local .beads directory.
func resolveAgentTrackingBeadsDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return resolveAgentTrackingBeadsDirFrom(cwd, os.Getenv("BEADS_DIR"))
}

// resolveAgentTrackingBeadsDirFrom is resolveAgentTrackingBeadsDir from cwd
// with the inherited BEADS_DIR envBeadsDir.
func resolveAgentTrackingBeadsDirFrom(cwd, envBeadsDir string) (string, error) {
	workDir, err := findBeadsWorkDirFrom(cwd)
	if err != nil {
		workDir, err = localBeadsWorkDir(cwd, envBeadsDir)
	}
	if err != nil {
		return "", err
	}

	beadsDir := beads.ResolveBeadsDir(workDir)
	if beadsDir == "" {
		return "", fmt.Errorf("not in a beads workspace")
	}
	return beadsDir, nil
}

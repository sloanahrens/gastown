package daemon

import (
	"os"
	"path/filepath"
	"strings"
)

// Moved here from internal/deacon when the deacon was deleted
// (gt-4k3fj.6.1): the convoy manager's dead-holder check is its only caller.

// isUnsafePathComponent reports whether s cannot safely be joined into a
// filesystem path as a single component: empty, or a "." / ".." traversal
// segment. Assignees are system-written, but this drives a git push
// (the convoy manager's dead-holder check), so a rig or agent
// name like ".." must not resolve outside the town root.
func isUnsafePathComponent(s string) bool {
	return s == "" || s == "." || s == ".."
}

// assigneeToWorktreePath resolves an assignee address to its git worktree path.
// Returns "" if the assignee format is unrecognized or the worktree doesn't exist.
// Supports polecat format "rig/polecats/name" and crew format "rig/crew/name".
func assigneeToWorktreePath(townRoot, assignee string) string {
	parts := strings.Split(assignee, "/")
	if len(parts) != 3 {
		return ""
	}

	rigName, agentType, name := parts[0], parts[1], parts[2]
	if agentType != "polecats" && agentType != "crew" {
		return ""
	}
	if isUnsafePathComponent(rigName) || isUnsafePathComponent(name) {
		return ""
	}

	rigPath := filepath.Join(townRoot, rigName)

	// New structure: rig/polecats/<name>/<rigname>/
	newPath := filepath.Join(rigPath, agentType, name, rigName)
	if info, err := os.Stat(newPath); err == nil && info.IsDir() {
		if _, err := os.Stat(filepath.Join(newPath, ".git")); err == nil {
			return newPath
		}
	}

	// Old structure: rig/polecats/<name>/
	oldPath := filepath.Join(rigPath, agentType, name)
	if info, err := os.Stat(oldPath); err == nil && info.IsDir() {
		if _, err := os.Stat(filepath.Join(oldPath, ".git")); err == nil {
			return oldPath
		}
	}

	return ""
}

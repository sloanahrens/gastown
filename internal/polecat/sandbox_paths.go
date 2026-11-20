package polecat

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/session"
)

// sandboxWorktreeFromEnv is the real worktree path a sandbox backend reports
// for a session: the value of GT_POLECAT_PATH in the prepared environment. The
// prepared work dir cannot stand in for it, because a backend may make it a
// link path every seat shares (gt-f80lq2). A prepared environment that names
// no GT_POLECAT_PATH therefore has no worktree to map, and the launch refuses
// it rather than place the sandbox town root where nothing exists.
func sandboxWorktreeFromEnv(env map[string]string) (string, error) {
	if p := env["GT_POLECAT_PATH"]; p != "" {
		return p, nil
	}
	return "", errors.New("the prepared env names no GT_POLECAT_PATH: no sandbox town root")
}

// sandboxTownRoot is the town root inside a sandbox, derived from where
// Prepare put the worktree: the host worktree sits at <hostTownRoot>/<rel>
// and the sandbox's at <sandboxTownRoot>/<rel>, so the sandbox root is the
// sandbox work dir minus that tail. A work dir that does not end in the same
// tail has no mapping, and the launch must stop before it starts an agent
// whose paths would not exist.
func sandboxTownRoot(hostTownRoot, hostWorkDir, sandboxWorkDir string) (string, error) {
	rel, err := filepath.Rel(hostTownRoot, hostWorkDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("worktree %s is not inside town %s", hostWorkDir, hostTownRoot)
	}
	tail := "/" + filepath.ToSlash(rel)
	root, ok := strings.CutSuffix(sandboxWorkDir, tail)
	if !ok || root == "" {
		return "", fmt.Errorf("sandbox work dir %s does not end in %s: no sandbox town root", sandboxWorkDir, tail)
	}
	return root, nil
}

// rewriteTownRootInPathEnv moves the host town root to the sandbox's in the
// env values that are paths or path lists. A value that is free text, a name
// or a flag is left as it is, even when it mentions the root.
func rewriteTownRootInPathEnv(env map[string]string, hostTownRoot, sandboxTownRoot string) {
	for k, v := range env {
		if isPathValue(v) {
			env[k] = session.RewriteTownRoot(v, hostTownRoot, sandboxTownRoot)
		}
	}
}

// isPathValue reports whether some element of a list separated by
// filepath.ListSeparator is an absolute path, so a path list such as
// "bin:/h/gt/bin" counts even though its first element is not one.
func isPathValue(v string) bool {
	for _, elem := range strings.Split(v, string(filepath.ListSeparator)) {
		if strings.HasPrefix(elem, "/") {
			return true
		}
	}
	return false
}

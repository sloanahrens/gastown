package polecat

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/session"
)

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

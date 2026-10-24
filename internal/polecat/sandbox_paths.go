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
// env values that are paths (they start with '/'). A value that is free text,
// a name or a flag list is left as it is, even when it mentions the root.
func rewriteTownRootInPathEnv(env map[string]string, hostTownRoot, sandboxTownRoot string) {
	for k, v := range env {
		if strings.HasPrefix(v, "/") {
			env[k] = session.RewriteTownRoot(v, hostTownRoot, sandboxTownRoot)
		}
	}
}

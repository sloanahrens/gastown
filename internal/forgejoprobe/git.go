package forgejoprobe

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Git is the seam the probe runs git through: dir is the working directory
// (empty for the process's own), env is added to the child's environment, and
// args are git's arguments. A failure carries git's stderr, so the caller can
// name what git refused.
type Git interface {
	Run(dir string, env []string, args ...string) (stdout string, err error)
}

// tokenVars are the variables a git child never inherits: the probe's
// credentials, which the pre-push hook of the rig would otherwise see.
var tokenVars = []string{"GT_AUTH_FORGEJO_TOKEN", "FORGEJO_ADMIN_TOKEN"}

// execGit is the production Git: it execs git with the process environment,
// minus the probe's tokens. run starts the configured command; it is the one
// process seam, so a test drives both failure paths without a subprocess.
type execGit struct {
	run func(*exec.Cmd) error
}

// NewGit returns the production Git.
func NewGit() Git {
	return execGit{run: func(cmd *exec.Cmd) error { return cmd.Run() }}
}

// Run execs git. The child's environment is the process environment with the
// token variables removed, then env laid over it.
func (g execGit) Run(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(append(os.Environ(), env...))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := g.run(cmd); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.String(), fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// gitEnv drops the token variables from environ. It is the one place the
// child environment is built, so a test can pin what git inherits without
// starting git.
func gitEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !isTokenVar(kv) {
			out = append(out, kv)
		}
	}
	return out
}

// isTokenVar reports whether one KEY=VALUE entry names a token variable.
func isTokenVar(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	for _, name := range tokenVars {
		if key == name {
			return true
		}
	}
	return false
}

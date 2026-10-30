package forgejoprobe

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// Git is the seam the probe runs git through: dir is the working directory
// (empty for the process's own), env is added to the child's environment, and
// args are git's arguments. A failure carries git's stderr, so the caller can
// name what git refused.
type Git interface {
	Run(dir string, env []string, args ...string) (stdout string, err error)
}

// scrubbedVars are the variables that point git at a repository. A git child
// never inherits them: run under a hook that runs the probe, they would send
// the child's git to a repository other than the one it was given.
var scrubbedVars = []string{"GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE"}

// execGit is the production Git: it execs git with the process environment,
// minus the credentials and the repository pins. run starts the configured
// command; it is the one process seam, so a test drives both failure paths
// without a subprocess.
type execGit struct {
	run func(*exec.Cmd) error
}

// NewGit returns the production Git.
func NewGit() Git {
	return execGit{run: func(cmd *exec.Cmd) error { return cmd.Run() }}
}

// Run execs git. The child's environment is the process environment with the
// scrubbed variables removed, then env laid over it.
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

// gitEnv drops the scrubbed variables from environ. It is the one place the
// child environment is built, so a test can pin what git inherits without
// starting git. HOME and PATH are inherited as they are: the rig's pre-push
// hook, ssh and the credential helpers need the real ones.
func gitEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !isScrubbed(kv) {
			out = append(out, kv)
		}
	}
	return out
}

// isScrubbed reports whether one KEY=VALUE entry must not reach a git child: a
// credential, whose name ends in _TOKEN, or a variable that pins the repository.
func isScrubbed(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	return strings.HasSuffix(key, "_TOKEN") || slices.Contains(scrubbedVars, key)
}

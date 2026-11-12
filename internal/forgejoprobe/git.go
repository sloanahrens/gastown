package forgejoprobe

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// Git is the seam the probe runs git through: dir is the working directory
// (empty for the process's own), env is added to the child's environment, and
// args are git's arguments. A failure carries git's stderr, so the caller can
// name what git refused.
type Git interface {
	Run(dir string, env []string, args ...string) (stdout string, err error)
}

// execGit is the production Git: it execs git with the process environment,
// minus the credentials and the repository pins. run starts the configured
// command; it is the one process seam, so a test drives both failure paths
// without a subprocess.
type execGit struct {
	run     func(*exec.Cmd) error
	environ func() []string // the environment a child inherits; nil means os.Environ
}

// NewGit returns the production Git.
func NewGit() Git {
	return execGit{run: func(cmd *exec.Cmd) error { return cmd.Run() }}
}

// Run execs git. The child's environment is the process environment, then env
// laid over it, with the scrubbed variables removed by forgejoprov.GitChildEnv.
func (g execGit) Run(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	inherited := os.Environ
	if g.environ != nil {
		inherited = g.environ
	}
	cmd.Env = forgejoprov.GitChildEnv(append(inherited(), env...))
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

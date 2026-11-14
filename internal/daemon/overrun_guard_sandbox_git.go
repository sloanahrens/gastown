package daemon

import (
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/git"
)

// errSandboxGitStdin is the refusal a call that feeds git on stdin gets: the
// in-sandbox runner runs one command per call and has no stdin stream to fill.
var errSandboxGitStdin = errors.New("the in-sandbox git runner cannot carry stdin")

// sandboxGitExec adapts a runner from session.SandboxGitRunner to the
// git.ExecFunc that git.NewGitWithExec runs every call through. Each
// git.ExecCall becomes one runner call with the call's directory and
// arguments, and the runner's output becomes the call's stdout. The runner
// reports no stderr and carries no stdin; it bounds each command itself, and
// the sandbox clone holds its own credentials, so the call's environment,
// unset keys and deadline are not passed on. A call that does carry stdin is
// refused before the runner is called.
func sandboxGitExec(run func(dir string, args ...string) ([]byte, error)) git.ExecFunc {
	return func(c git.ExecCall) (string, string, error) {
		if c.Stdin() != "" {
			if args := c.Args(); len(args) > 0 {
				return "", "", fmt.Errorf("%w (git %s)", errSandboxGitStdin, args[0])
			}
			return "", "", errSandboxGitStdin
		}
		out, err := run(c.Dir(), c.Args()...)
		return string(out), "", err
	}
}

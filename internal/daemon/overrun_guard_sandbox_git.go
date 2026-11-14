package daemon

import (
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/git"
)

// errSandboxGitStdin is the refusal a call that feeds git on stdin gets: the
// in-sandbox runner runs one command per call and has no stdin stream to fill.
var errSandboxGitStdin = errors.New("the in-sandbox git runner cannot carry stdin")

// sandboxGitRunError is the error sandboxGitExec returns when the runner fails.
// git.ExecFunc's error must answer an exit status through ExitCode() int, and
// the runner's signature cannot carry one, so this error answers for it: a
// runner error that carries an exit status keeps it, and one that carries none
// reports -1, the value git's exitCode reads as "no status" (the command did
// not start, or was killed). Unwrap keeps the runner's own error matchable, so
// ErrNothingToPush and a deadline kill reach the caller unchanged.
type sandboxGitRunError struct{ err error }

func (e *sandboxGitRunError) Error() string { return e.err.Error() }
func (e *sandboxGitRunError) Unwrap() error { return e.err }

// ExitCode is the runner error's own exit status when it carries one, else -1:
// the sandbox runner cannot tell git's non-zero status from a transport
// failure, and -1 makes a Git method return the failure rather than read it as
// git's answer (a missing ref is exit 1, not an error).
func (e *sandboxGitRunError) ExitCode() int {
	var coded interface{ ExitCode() int }
	if errors.As(e.err, &coded) {
		return coded.ExitCode()
	}
	return -1
}

// sandboxGitExec adapts a runner from session.SandboxGitRunner to the
// git.ExecFunc that git.NewGitWithExec runs every call through. Each
// git.ExecCall becomes one runner call with the call's directory and
// arguments, and the runner's output becomes the call's stdout. The runner
// reports no stderr and carries no stdin; it bounds each command itself, and
// the sandbox clone holds its own credentials, so the call's environment,
// unset keys and deadline are not passed on. A call that does carry stdin is
// refused before the runner is called. A runner failure comes back as a
// *sandboxGitRunError, which states the exit status the runner could not
// carry.
func sandboxGitExec(run func(dir string, args ...string) ([]byte, error)) git.ExecFunc {
	return func(c git.ExecCall) (string, string, error) {
		if c.Stdin() != "" {
			if args := c.Args(); len(args) > 0 {
				return "", "", fmt.Errorf("%w (git %s)", errSandboxGitStdin, args[0])
			}
			return "", "", errSandboxGitStdin
		}
		out, err := run(c.Dir(), c.Args()...)
		if err != nil {
			return string(out), "", &sandboxGitRunError{err: err}
		}
		return string(out), "", nil
	}
}

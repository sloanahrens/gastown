package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// gitCall is one git invocation: the arguments after "git" (any --git-dir
// prefix already applied), the directory it runs in ("" means the process
// cwd), extra environment on top of the process's own, stdin, and a deadline
// (0 means none).
type gitCall struct {
	dir     string
	args    []string
	env     []string
	stdin   string
	timeout time.Duration
}

// runFunc runs one git invocation and returns what it wrote to stdout and
// stderr. Git sends every git process through one, so tests can record and
// answer them without starting git. A call that ran out its timeout returns
// an error wrapping errTimedOut; a call that exited non-zero returns an error
// with an ExitCode() method (see exitCode).
type runFunc func(c gitCall) (stdout, stderr string, err error)

// errTimedOut is wrapped by a runFunc's error when a call ran out its
// timeout and was killed.
var errTimedOut = errors.New("git timed out")

// runner returns the runFunc g sends git through: the real git on PATH
// unless a test set one.
func (g *Git) runner() runFunc {
	if g.exec == nil {
		return realRun
	}
	return g.exec
}

// realRun runs git on PATH.
//
// A call with a timeout is killable as a whole (boundRemoteCommand), and a
// call with a timeout or extra environment (a push, which may fork a remote
// helper) captures its output in files rather than pipes (runCapturingOutput).
// Every other call runs in its own detached process group with pipes.
func realRun(c gitCall) (string, string, error) {
	ctx := context.Background()
	var cancel context.CancelFunc
	if c.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", c.args...)
	cmd.Dir = c.dir
	if len(c.env) > 0 {
		cmd.Env = append(os.Environ(), c.env...)
	}
	if c.stdin != "" {
		cmd.Stdin = strings.NewReader(c.stdin)
	}
	if c.timeout > 0 {
		boundRemoteCommand(cmd)
	} else {
		util.SetDetachedProcessGroup(cmd)
	}

	var stdout, stderr string
	var err error
	if c.timeout > 0 || len(c.env) > 0 {
		stdout, stderr, err = runCapturingOutput(cmd)
	} else {
		var out, errOut bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errOut
		err = cmd.Run()
		stdout, stderr = out.String(), errOut.String()
	}
	if err != nil && c.timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stdout, stderr, errTimedOut
	}
	return stdout, stderr, err
}

// exitCode returns the exit status err carries, or -1 when it carries none
// (git did not start, or was killed). It matches any error with an
// ExitCode() method, so a test runner's errors work as well as
// *exec.ExitError.
func exitCode(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}

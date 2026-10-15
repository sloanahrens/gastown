// Package pushcmd runs one operator-configured notify command: an argv array,
// the message on stdin, a hard deadline.
//
// The command is the operator's, so nothing about it is trusted to behave. It
// gets no shell, an environment of PATH and HOME only, no stdout or stderr to
// fill, and a process group that is killed when the deadline passes. Errors
// carry what went wrong and never the argv, because an argv can hold a token
// (a URL with a topic or key in it) that has no business in a log.
package pushcmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// Timeout is how long a notify command may run before its process group is
// killed.
const Timeout = 10 * time.Second

// pipeGrace is how long a finished command may leave its stdin pipe held open
// by a grandchild before Wait gives up on it.
const pipeGrace = 2 * time.Second

// Run runs argv with stdin as its standard input and reports how it ended.
// Output is discarded. A command still running after timeout is killed along
// with its whole process group, and the error says so.
func Run(argv []string, stdin string, timeout time.Duration) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("no command configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: the operator's own configured command, run without a shell
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = environ()
	cmd.WaitDelay = pipeGrace
	util.SetProcessGroup(cmd)

	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// The command succeeded; only a grandchild kept the pipe open.
		return nil
	}
	return describe(ctx, cmd, err, timeout)
}

// environ is the whole environment a notify command sees. A pager that needs a
// secret reads it from a file under HOME; it does not inherit the daemon's.
func environ() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
}

// describe turns what exec returned into an error that leaves the argv out:
// exec's own messages name the program path.
func describe(ctx context.Context, cmd *exec.Cmd, err error, timeout time.Duration) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s", timeout)
	}
	if cmd.ProcessState != nil {
		return fmt.Errorf("command failed: %s", cmd.ProcessState)
	}
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return errors.New("program not found")
	case errors.Is(err, fs.ErrPermission):
		return errors.New("program not executable")
	default:
		return errors.New("could not start the command")
	}
}

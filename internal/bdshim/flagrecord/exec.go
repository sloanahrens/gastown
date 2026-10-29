package flagrecord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// runLimit bounds one bd run, so a hung bd fails the recording or the doctor
// check instead of stalling it.
const runLimit = time.Minute

// ExecRunner returns a Runner that runs the bd binary bin. The bd target
// variables are stripped from the environment, so bd selects the database in
// the directory it runs in, not the operator's town.
func ExecRunner(bin string) Runner {
	return func(dir string, args []string) ([]byte, int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: bin is the bd binary the operator names; args are the recording's fixed rows and help words
		c.Dir = dir
		c.Env = beads.StripBDTargetEnv(os.Environ())
		c.WaitDelay = time.Second
		var stdout bytes.Buffer
		c.Stdout = &stdout
		c.Stderr = os.Stderr
		err := c.Run()
		return exitResult(stdout.Bytes(), err, errors.Is(ctx.Err(), context.DeadlineExceeded))
	}
}

// exitResult turns how a bd run ended into a recording's stdout and exit code.
// A non-zero exit is a result; a run that timed out or was killed, or that
// never started, is an error, so its partial output is never read as bd's
// answer. Any error carrying an exit code counts as bd's exit, as
// *exec.ExitError does in production and as a test's fake error does
// (gt-p85pw.87).
func exitResult(stdout []byte, runErr error, timedOut bool) ([]byte, int, error) {
	if timedOut {
		return nil, 0, fmt.Errorf("bd ran past %s", runLimit)
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(runErr, &exitErr) {
		if exitErr.ExitCode() < 0 {
			return nil, 0, errors.New("bd was killed by a signal")
		}
		return stdout, exitErr.ExitCode(), nil
	}
	if runErr != nil {
		return nil, 0, runErr
	}
	return stdout, 0, nil
}

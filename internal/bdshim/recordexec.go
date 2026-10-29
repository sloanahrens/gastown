package bdshim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/bdshim/flagrecord"
	"github.com/steveyegge/gastown/internal/beads"
)

// The limits one recording runs under. runLimit caps a single bd run, so a
// hung bd fails the recording or the doctor check instead of stalling it;
// recordBudget caps the walk as a whole, so a bd that answers each help slowly
// cannot hold a doctor run command after command. Together they bound the
// check's work: bd 1.2.2 documents a few hundred help pages, well inside one
// budget when bd answers at all.
const (
	runLimit     = time.Minute
	recordBudget = 2 * time.Minute
)

// ExecRunner returns a flagrecord.Runner that runs the bd binary bin under the
// recording's limits. The bd target variables are stripped from the
// environment, so bd selects the database in the directory it runs in, not the
// operator's town.
func ExecRunner(bin string) flagrecord.Runner {
	return execRunner(bin, time.Now)
}

// execRunner is ExecRunner with the clock injected, so a test spends the
// budget without waiting on one.
func execRunner(bin string, now func() time.Time) flagrecord.Runner {
	deadline := now().Add(recordBudget)
	return func(dir string, args []string) ([]byte, int, error) {
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return nil, 0, fmt.Errorf("bd's help took longer than the %s one recording allows", recordBudget)
		}
		limit := min(remaining, runLimit)
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: bin is the bd binary the operator names; args are the recording's fixed rows and help words
		c.Dir = dir
		c.Env = beads.StripBDTargetEnv(os.Environ())
		c.WaitDelay = time.Second
		var stdout bytes.Buffer
		c.Stdout = &stdout
		c.Stderr = os.Stderr
		err := c.Run()
		return exitResult(stdout.Bytes(), err, errors.Is(ctx.Err(), context.DeadlineExceeded), limit)
	}
}

// exitResult turns how a bd run ended into a recording's stdout and exit code.
// A non-zero exit is a result; a run that timed out or was killed, or that
// never started, is an error, so its partial output is never read as bd's
// answer. Any error carrying an exit code counts as bd's exit, as
// *exec.ExitError does in production and as a test's fake error does
// (gt-p85pw.87).
func exitResult(stdout []byte, runErr error, timedOut bool, limit time.Duration) ([]byte, int, error) {
	if timedOut {
		return nil, 0, fmt.Errorf("bd ran past %s", limit)
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

package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/util"
)

// sideCmd builds the command a side load runs in dir, with env as its whole environment.
func sideCmd(ctx context.Context, dir string, env []string, argv []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("side run: empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	return cmd, nil
}

// ExecSideRun runs the command sideCmd builds in dir and returns its exit code.
// A command that exits by itself with a non-zero status gives that status and a
// nil error, so a failing presubmit is recorded as a side run. A signal, a
// canceled ctx and a command that cannot start give -1 and an error.
func ExecSideRun(ctx context.Context, dir string, env []string, argv []string) (int, error) {
	cmd, err := sideCmd(ctx, dir, env, argv)
	if err != nil {
		return -1, err
	}
	// Killing only the command's own pid leaves the compilers make started: the group ends the tree.
	util.SetProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		} else {
			// A command that exits by itself reports its own status; a signal
			// reports -1 from ExitCode, so only a status is a code.
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
				return exitErr.ExitCode(), nil
			}
		}
		return -1, fmt.Errorf("side run %s: %w", strings.Join(argv[:min(len(argv), 2)], " "), err)
	}
	return 0, nil
}

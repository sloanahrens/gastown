package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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

// ExecSideRun runs the command sideCmd builds in dir and returns its exit code, or -1 and an error on every failure.
func ExecSideRun(ctx context.Context, dir string, env []string, argv []string) (int, error) {
	cmd, err := sideCmd(ctx, dir, env, argv)
	if err != nil {
		return -1, err
	}
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		return -1, fmt.Errorf("side run %s: %w", strings.Join(argv[:min(len(argv), 2)], " "), err)
	}
	return 0, nil
}

package rehearsal

import (
	"context"
	"errors"
	"os/exec"
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

package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// productionPodExec is the real podExec: it runs argv with exec.CommandContext,
// stdin set from the bytes, and returns what the child printed on stdout, kept
// apart from stderr, and its exit status. Stderr is never parsed and never mixed
// into the reply: update.RunFunc returns both combined, and a kubectl warning
// there would corrupt the one-word replies of the landing-hold scripts. It is
// discarded here; what a failure says is the next bead's.
//
// A child that exits non-zero is its status with the stdout so far and no error,
// the way update.RunFunc reports it. A command that cannot start, or a child
// killed by the context, is an error naming the command.
func productionPodExec(ctx context.Context, argv []string, stdin []byte) ([]byte, int, error) {
	if len(argv) == 0 {
		return nil, 0, errors.New("no argv to run")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), 0, nil
	case ctx.Err() != nil:
		return nil, 0, fmt.Errorf("running %s: %w", argv[0], ctx.Err())
	case errors.As(err, &exit):
		return stdout.Bytes(), exit.ExitCode(), nil
	default:
		return nil, 0, fmt.Errorf("starting %s: %w", argv[0], err)
	}
}

package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// productionPodExec is the real podExec: it runs argv with exec.CommandContext,
// stdin set from the bytes, and returns what the child printed on stdout, kept
// apart from stderr, and its exit status. Stderr is never parsed and never mixed
// into the reply: update.RunFunc returns both combined, and a kubectl warning
// there would corrupt the one-word replies of the landing-hold scripts. It is
// discarded here (io.Discard); the bounded tail of it that an error carries is
// the next bead's (gt-gb3ola).
//
// A child that exits non-zero is its status with the stdout so far and no error,
// the way update.RunFunc reports it. A command that cannot start, a child killed
// by the context and a child killed by a signal (no exit status) are errors
// naming the command.
func productionPodExec(ctx context.Context, argv []string, stdin []byte) ([]byte, int, error) {
	if len(argv) == 0 {
		return nil, 0, errors.New("no argv to run")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), 0, nil
	case ctx.Err() != nil:
		return nil, 0, fmt.Errorf("running %s: %w", argv[0], ctx.Err())
	case errors.As(err, &exit):
		if exit.ExitCode() < 0 {
			return nil, 0, fmt.Errorf("running %s: %w", argv[0], err)
		}
		return stdout.Bytes(), exit.ExitCode(), nil
	default:
		return nil, 0, fmt.Errorf("starting %s: %w", argv[0], err)
	}
}

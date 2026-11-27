package sbxcreds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// ExecRunner runs argv as a program and returns its stdout. A program that cannot be started is an
// error naming it. A non-zero exit is an error naming the program and its exit status: the output
// is never in that error, because a kubeadm call prints a kubeconfig. Its stderr is discarded for
// the same reason. Every error returns no output.
func ExecRunner(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("no program to run: the argv is empty")
	}
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("%s exited: %w", argv[0], exitErr)
		}
		return nil, fmt.Errorf("could not start %s: %w", argv[0], err)
	}
	return stdout.Bytes(), nil
}

package rehearsal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ExecRun runs argv in the process's working directory and returns its stdout
// alone; stderr is discarded. A failed command gives an error that names the
// first two words of argv and wraps the cause with %w. The error carries no
// captured output, since a command's output can hold a secret. An empty argv
// is an error and runs nothing.
func ExecRun(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("run: empty argv")
	}
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("run %s: %w", strings.Join(argv[:min(len(argv), 2)], " "), err)
	}
	return stdout.Bytes(), nil
}

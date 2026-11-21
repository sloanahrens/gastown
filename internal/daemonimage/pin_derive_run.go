package daemonimage

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// goListRunner runs go in dir with args and returns its standard output. It is
// the seam the guard's derivation is tested through.
type goListRunner func(dir string, args ...string) ([]byte, error)

// deriveGoListArgs is the argument list of the command that derives the devops
// input directories: every package the three tools import, as a directory.
func deriveGoListArgs() []string {
	return []string{"list", "-deps", "-f", "{{.Dir}}", "./cmd/reporter", "./cmd/release-reporter", "./cmd/cloudcheck"}
}

// runGoList runs the derivation through run in dir.
func runGoList(run goListRunner, dir string) ([]byte, error) {
	return run(dir, deriveGoListArgs()...)
}

// execGoList runs go in dir. A failure carries go's own stderr text.
func execGoList(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...) //nolint:gosec // G204: a fixed binary and the fixed derivation arguments
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, goListFailure(dir, args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// goListFailure is the error of a go run that failed: the call, the directory,
// the exit error and go's own stderr text.
func goListFailure(dir string, args []string, err error, stderr string) error {
	return fmt.Errorf("go %s in %s: %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(stderr))
}

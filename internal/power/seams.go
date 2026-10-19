package power

import (
	"bytes"
	"context"
	"net/http"
	"os/exec"
	"time"
)

// runCommand runs one command with its working directory set and returns its
// stdout; a failure carries stderr after it for the error message. The
// streams stay apart on success so a warning on stderr cannot corrupt the
// JSON read from stdout (gt polecat list --json).
func runCommand(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return stdout.String() + stderr.String(), err
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// executableLookPath is exec.LookPath, kept behind a seam so a test can vary
// what is installed.
func executableLookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// httpStatus probes a URL and returns its status code.
func httpStatus(url string) (int, error) {
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

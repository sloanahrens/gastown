package presubmit

import (
	"errors"
	"fmt"
)

// RedBaseGoResult returns out for a go test run that exited, whatever its status, and an error wrapping err for a run that did not happen or was killed, so a killed run never reads as tests that failed (gt-kwurob).
func RedBaseGoResult(out []byte, err error) ([]byte, error) {
	if err == nil {
		return out, nil
	}
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) {
		return nil, fmt.Errorf("go test did not run: %w", err)
	}
	if exit.ExitCode() < 0 {
		return nil, fmt.Errorf("go test was killed by a signal: %w", err)
	}
	return out, nil
}

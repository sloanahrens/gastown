// Package sigkill is a budget-runner fixture: its test binary kills itself
// with SIGKILL, so go test must report "signal: killed" for it.
package sigkill

import (
	"os"
	"syscall"
	"testing"
)

func TestKilled(t *testing.T) {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

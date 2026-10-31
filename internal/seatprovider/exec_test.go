package seatprovider

import (
	"context"
	"os/exec"
	"testing"
)

func TestExecRunnerReportsACommandThatCannotStart(t *testing.T) {
	t.Parallel()
	// A zero Cmd has no path, so Run fails before any process starts.
	r := ExecRunner{Command: func(string, ...string) *exec.Cmd { return &exec.Cmd{} }}
	if _, err := r.Run(context.Background(), []string{"provider", "list"}, nil); err == nil {
		t.Fatal("ExecRunner returned no error for a command that cannot start, want the failure")
	}
}

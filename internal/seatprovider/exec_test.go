package seatprovider

import (
	"context"
	"os"
	"os/exec"
	"slices"
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

// The Command seam sees the program, the argv and the environment the child
// would get, so the test reads them off the *exec.Cmd without starting one.
func TestExecRunnerPassesTheArgvAndAddsTheEnvironment(t *testing.T) {
	t.Parallel()
	var gotName string
	var gotArgs []string
	var cmd *exec.Cmd
	r := ExecRunner{Command: func(name string, args ...string) *exec.Cmd {
		gotName, gotArgs = name, slices.Clone(args)
		cmd = &exec.Cmd{} // no path: Run fails before any process starts
		return cmd
	}}
	argv := []string{"provider", "create", "--name", "x"}
	_, _ = r.Run(context.Background(), argv, []string{"TRACKER_TOKEN=secret-value"})

	if gotName != "openshell" || !slices.Equal(gotArgs, argv) {
		t.Errorf("command = %q %q, want openshell %q", gotName, gotArgs, argv)
	}
	if !slices.Contains(cmd.Env, "TRACKER_TOKEN=secret-value") {
		t.Errorf("child environment lacks the added variable: %q", cmd.Env)
	}
	if len(cmd.Env) != len(os.Environ())+1 {
		t.Errorf("child environment has %d entries, want this process's %d plus the one added", len(cmd.Env), len(os.Environ()))
	}
	for _, a := range gotArgs {
		if a == "secret-value" {
			t.Errorf("the token reached argv: %q", gotArgs)
		}
	}
}

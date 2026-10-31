package seatprovider

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
)

// ExecRunner is the production Runner: it runs the provider CLI in a child
// process and returns its stdout.
type ExecRunner struct {
	// Command builds the command to run; nil is exec.Command. A test swaps in a
	// command that cannot start, so its failure path runs with no process.
	Command func(name string, args ...string) *exec.Cmd
}

// Run runs argv through os/exec with env added to this process's environment.
// The child's stderr is dropped rather than returned, because openshell can echo
// the credential it was given.
func (r ExecRunner) Run(ctx context.Context, argv []string, env []string) ([]byte, error) {
	command := r.Command
	if command == nil {
		command = exec.Command
	}
	cmd := command(openshellBinary, argv...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = io.Discard
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	return stdout.Bytes(), err
}

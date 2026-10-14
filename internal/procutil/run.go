// Package procutil runs external commands so that every child the caller
// starts is waited on, whichever way the command ends.
package procutil

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// pipeGrace bounds how long Wait lingers on a stdout or stderr pipe that a
// grandchild still holds open after the command itself has exited. The child is
// reaped either way; without a bound, the call hangs until the grandchild goes.
const pipeGrace = 2 * time.Second

// Output runs name with args and returns its stdout; it always Waits on the
// child, on success, failure, timeout and context cancel, so no defunct process
// outlives the call.
//
// A timeout above zero bounds the run, and the context bounds it too; either
// one kills the child as exec.CommandContext does. The error then wraps
// ctx.Err() (context.DeadlineExceeded or context.Canceled) and, when the
// child had started, the *exec.ExitError. A failed command's stderr is on that
// ExitError.
func Output(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: callers pass fixed tool names
	cmd.WaitDelay = pipeGrace
	out, err := cmd.Output() // Start then Wait: the child is reaped before this returns
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("%s: %w", name, errors.Join(ctx.Err(), err))
	}
	return out, err
}

// Start launches name with args and returns at once, for a command the caller
// does not wait on (a browser open). A goroutine Waits on the child, so it is
// reaped when it exits; the returned channel yields Wait's result once and is
// buffered, so a caller that ignores it leaks nothing. A start failure returns
// a nil channel and the error, with no child to reap.
func Start(name string, args ...string) (<-chan error, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // G204: callers pass fixed tool names
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done, nil
}

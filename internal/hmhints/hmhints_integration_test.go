//go:build integration

package hmhints

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// TestIntegrationExecRunnerRefusesOversizedOutput: 2 MiB is twice the bound.
// An uncapped runner reads it all and reports success, so this fails without
// the cap.
func TestIntegrationExecRunnerRefusesOversizedOutput(t *testing.T) {
	t.Parallel()
	out, err := ExecRunner(context.Background(), []string{"dd", "if=/dev/zero", "bs=1024", "count=2048"})
	if !errors.Is(err, errOutputTooLong) {
		t.Fatalf("ExecRunner error = %v, want errOutputTooLong in its chain", err)
	}
	if out != nil {
		t.Fatalf("ExecRunner returned %d bytes beside the error, want none", len(out))
	}
}

// TestIntegrationExecRunnerDoesNotWaitForAHeldPipe: a child holding hm's
// stdout outlives hm, so only the wait delay can end the run before that child
// exits. The context row also ignores SIGTERM, so a cancel that waits out a
// kill grace before escalating takes that grace too.
func TestIntegrationExecRunnerDoesNotWaitForAHeldPipe(t *testing.T) {
	t.Parallel()
	const (
		hold        = 4 * time.Second
		ctxDeadline = 100 * time.Millisecond
	)
	holdScript := fmt.Sprintf("sleep %d", int(hold/time.Second))
	tests := []struct {
		name     string
		script   string
		ctx      func() (context.Context, context.CancelFunc)
		wantErr  bool
		maxTaken time.Duration
	}{
		{
			name:   "hm exits zero and a child holds the pipe",
			script: holdScript + " & exit 0",
			ctx:    func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			// hm exits at once; the wait delay is the whole bound.
			maxTaken: execWaitDelay + time.Second,
		},
		{
			name:   "ctx ends while a child holds the pipe",
			script: "trap '' TERM; " + holdScript + " & wait",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), ctxDeadline)
			},
			wantErr: true,
			// The context plus the wait delay, and well inside
			// util.ProcessGroupKillGrace (2s), which a cancel that escalates
			// would spend.
			maxTaken: ctxDeadline + execWaitDelay + 500*time.Millisecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := tt.ctx()
			defer cancel()
			start := time.Now()
			_, err := ExecRunner(ctx, []string{"sh", "-c", tt.script})
			elapsed := time.Since(start)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ExecRunner error = %v, want error %v", err, tt.wantErr)
			}
			if elapsed > tt.maxTaken {
				t.Fatalf("ExecRunner took %v, want at most %v (a child held the pipe for %v)", elapsed, tt.maxTaken, hold)
			}
		})
	}
}

// TestIntegrationExecRunnerKeepsStderr: hm's error text is on the returned
// exit error's Stderr, and not in the error's message, which a caller may log.
func TestIntegrationExecRunnerKeepsStderr(t *testing.T) {
	t.Parallel()
	_, err := ExecRunner(context.Background(), []string{"sh", "-c", "echo 'hm: index is corrupt' >&2; exit 1"})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("ExecRunner error = %v, want an exec.ExitError in its chain", err)
	}
	if got, want := string(exitErr.Stderr), "hm: index is corrupt\n"; got != want {
		t.Fatalf("ExitError.Stderr = %q, want %q", got, want)
	}
	if got := err.Error(); got != "exit status 1" {
		t.Fatalf("error message = %q, want only the exit status", got)
	}
}

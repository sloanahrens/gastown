//go:build integration

package hmhints

import (
	"context"
	"errors"
	"fmt"
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
// exits.
func TestIntegrationExecRunnerDoesNotWaitForAHeldPipe(t *testing.T) {
	t.Parallel()
	const hold = 4 * time.Second
	holdScript := fmt.Sprintf("sleep %d", int(hold/time.Second))
	tests := []struct {
		name    string
		script  string
		ctx     func() (context.Context, context.CancelFunc)
		wantErr bool
	}{
		{
			name:   "hm exits zero and a child holds the pipe",
			script: holdScript + " & exit 0",
			ctx:    func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
		},
		{
			name:   "ctx ends while a child holds the pipe",
			script: holdScript + " & wait",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 100*time.Millisecond)
			},
			wantErr: true,
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
			if elapsed > hold-1*time.Second {
				t.Fatalf("ExecRunner took %v with a child holding the pipe for %v", elapsed, hold)
			}
		})
	}
}

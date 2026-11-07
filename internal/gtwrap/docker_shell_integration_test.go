//go:build integration

package gtwrap

import (
	"context"
	"testing"
	"time"
)

// TestCaptureHoldsTheProbeBoundWhenAChildKeepsStdoutOpen runs a real process, which
// the unit tier refuses. The shell leaves a child holding stdout open and then
// runs past the probe's deadline. Capture must return near the deadline and
// refuse, not wait for the child to exit.
func TestIntegrationCaptureHoldsTheProbeBoundWhenAChildKeepsStdoutOpen(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	type result struct {
		exit int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		_, exit, err := OSShell{}.Capture(ctx, []string{"sh", "-c", "sleep 30 & sleep 30"})
		done <- result{exit, err}
	}()
	select {
	case got := <-done:
		// The deadline kills the shell with SIGKILL, which a shell reports as 128+9.
		if got.exit != 137 {
			t.Fatalf("Capture = exit %d, err %v; want 137, the status of a probe the deadline killed", got.exit, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Capture did not return: a child holding stdout open kept the probe past its bound")
	}
}

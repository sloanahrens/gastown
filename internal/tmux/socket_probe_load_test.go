//go:build !windows

package tmux

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The gt-h9z guard probes a live socket with `tmux list-sessions` before
// letting tmux start a server on it, because tmux unlinks and rebinds a
// socket path it cannot reach. It is the one tmux call with a deadline: a
// hijacked path may accept and never answer, and a create must not hang on
// it. The deadline is newSessionSocketProbeTimeout, sized for a healthy
// server on a loaded host (fcb4d1f: 1s refused real servers at load 30).

// probeRunner answers list-sessions after answerAfter of clock time, or never
// when answerAfter is 0; a probe still running at its deadline is killed.
func probeRunner(tm **Tmux, answerAfter time.Duration) execFunc {
	return func(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
		if answerAfter == 0 {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		select {
		case at := <-(*tm).clk().After(answerAfter):
			if dl, ok := ctx.Deadline(); ok && !at.Before(dl) {
				return nil, nil, context.DeadlineExceeded
			}
			return nil, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

// probeGuard runs ensureNewSessionSocketSafe on a live fake socket with the
// given list-sessions latency, and returns the running result and the clock.
func probeGuard(t *testing.T, answerAfter time.Duration, sleepers int) (<-chan error, func(time.Duration)) {
	t.Helper()
	fs := newFakeSockets()
	fs.set(guardPath, sockLive)
	var tm *Tmux
	tm = guardTmux(fs, newScripted(nil))
	tm.exec = probeRunner(&tm, answerAfter)
	clk := tm.clock.(interface {
		BlockUntilContext(context.Context, int) error
		Advance(time.Duration)
	})
	done := make(chan error, 1)
	go func() { done <- tm.ensureNewSessionSocketSafe() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := clk.BlockUntilContext(ctx, sleepers); err != nil {
		t.Fatalf("probe never waited on the clock: %v", err)
	}
	return done, clk.Advance
}

// TestSocketGuardAcceptsHealthyServerWithinTimeout: a server that answers
// just inside the deadline is a healthy one; the create goes ahead.
func TestSocketGuardAcceptsHealthyServerWithinTimeout(t *testing.T) {
	t.Parallel()
	done, advance := probeGuard(t, newSessionSocketProbeTimeout-time.Millisecond, 2)
	advance(newSessionSocketProbeTimeout - time.Millisecond)
	if err := <-done; err != nil {
		t.Fatalf("server answering within %s refused: %v", newSessionSocketProbeTimeout, err)
	}
}

// TestSocketGuardRefusesSilentListenerAtTimeout: a listener that never
// answers is refused, and not a moment before the deadline.
func TestSocketGuardRefusesSilentListenerAtTimeout(t *testing.T) {
	t.Parallel()
	done, advance := probeGuard(t, 0, 1)
	advance(newSessionSocketProbeTimeout - time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("refused before the deadline: %v", err)
	default:
	}
	advance(time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timed out after "+newSessionSocketProbeTimeout.String()) {
			t.Fatalf("silent listener = %v, want a timed-out refusal", err)
		}
	case <-ctx.Done():
		t.Fatal("guard still waiting after the deadline")
	}
}

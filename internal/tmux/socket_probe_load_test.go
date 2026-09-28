//go:build !windows

package tmux

import (
	"context"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// TestNewSessionSocketGuardToleratesSlowHealthyServer: under host load a
// healthy tmux server can take well over a second to answer the guard's
// list-sessions probe (the tmux client's own exec is what is slow). That is
// not the gt-h9z hijacked-socket case, and refusing it failed a real
// concurrent create at load 30 (TestNewSessionWithCommand_Concurrent,
// 2026-09-27: "list-sessions timed out after 1s").
func TestNewSessionSocketGuardToleratesSlowHealthyServer(t *testing.T) {
	t.Parallel()
	socket := uniqueSocketName(t, "gt-h9z-slow")
	listener, _ := listenOnSocketPath(t, socket)
	go acceptAndClose(listener)

	clk := clockwork.NewFakeClock()
	slow := func(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
		select {
		case at := <-clk.After(2 * time.Second): // answers, just late
			// A client still running at its deadline is killed; judge by the
			// time it answered, not by which channel the scheduler served first.
			if dl, ok := ctx.Deadline(); ok && !at.Before(dl) {
				return nil, nil, context.DeadlineExceeded
			}
			return nil, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	tm := newTmuxForTest(socket, slow, clk)
	done := make(chan error, 1)
	go func() { done <- tm.ensureNewSessionSocketSafe() }()
	// Two sleepers: the probe's deadline and the late answer. Move time once,
	// to the answer, so nothing else can fire in between.
	if err := clk.BlockUntilContext(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Second)
	if err := <-done; err != nil {
		t.Fatalf("slow but healthy tmux refused: %v", err)
	}
}

// TestNewSessionSocketGuardRefusesSilentListener keeps the gt-h9z bound: a
// listener that never answers is still refused once the probe gives up.
func TestNewSessionSocketGuardRefusesSilentListener(t *testing.T) {
	t.Parallel()
	socket := uniqueSocketName(t, "gt-h9z-silent")
	listener, _ := listenOnSocketPath(t, socket)
	go acceptAndClose(listener)

	clk := clockwork.NewFakeClock()
	silent := func(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	tm := newTmuxForTest(socket, silent, clk)
	done := make(chan error, 1)
	go func() { done <- tm.ensureNewSessionSocketSafe() }()
	if err := clk.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(newSessionSocketProbeTimeout)
	if err := <-done; err == nil {
		t.Fatal("silent listener accepted")
	}
}

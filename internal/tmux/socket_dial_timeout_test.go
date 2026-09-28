//go:build !windows

package tmux

import (
	"testing"

	"github.com/jonboulle/clockwork"
)

// dialTimeout is the error a dial that ran out of time returns (net.Error
// with Timeout() true). Under host load a dial to a socket nothing listens
// on can time out before the kernel's refusal is read.
type dialTimeout struct{}

func (dialTimeout) Error() string   { return "dial unix: i/o timeout" }
func (dialTimeout) Timeout() bool   { return true }
func (dialTimeout) Temporary() bool { return true }

// TestUnlinkDeadSocketFileRetriesTimedOutDial: a timed-out dial says nothing
// about the socket, so the cleanup keeps checking within socketUnlinkWait
// instead of leaving the stale file behind (the litter gt-20di removed).
func TestUnlinkDeadSocketFileRetriesTimedOutDial(t *testing.T) {
	t.Parallel()
	fs := newFakeSockets()
	fs.set("/s/gone", sockStale)
	fs.queueDial("/s/gone", dialTimeout{}, dialTimeout{})
	clk := newFixedClock()
	done := make(chan struct{}, 1)
	go func() {
		unlinkDeadSocketFile(clk, *fs.ops(), "/s/gone")
		done <- struct{}{}
	}()
	driveClock(t, clk, socketUnlinkInterval, done)
	if fs.state("/s/gone") != sockAbsent {
		t.Error("stale socket file survived a timed-out first dial")
	}
}

// TestSocketGuardRetriesTimedOutDial: the new-session guard must not refuse a
// create because one dial timed out under load; it re-dials within the probe
// budget and here finds the socket stale.
func TestSocketGuardRetriesTimedOutDial(t *testing.T) {
	t.Parallel()
	fs := newFakeSockets()
	fs.set(guardPath, sockStale)
	fs.queueDial(guardPath, dialTimeout{})
	tm := guardTmux(fs, newScripted(nil))
	clk := tm.clock.(*clockwork.FakeClock)
	if err := driven(t, clk, socketUnlinkInterval, tm.ensureNewSessionSocketSafe); err != nil {
		t.Fatalf("guard after one timed-out dial = %v, want nil (socket is stale)", err)
	}
}

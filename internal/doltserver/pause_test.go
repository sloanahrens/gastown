package doltserver

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltpause"
)

func pauseTown(t *testing.T, townRoot string, until time.Time) {
	t.Helper()
	if err := doltpause.Write(townRoot, doltpause.Marker{
		Actor: "deacon", Reason: "weekly gc", Until: until, Since: fakeEpoch,
	}); err != nil {
		t.Fatal(err)
	}
}

// While paused, Start refuses before touching anything, WaitForReady answers
// at once instead of spending its timeout, and an unreachable server is
// explained by the pause (gt-8z769.2).
func TestPausedTown_ClientsReportThePause(t *testing.T) {
	t.Parallel()
	f := newFakeHost().townPort(4541)
	h := f.host()
	townRoot := serverModeTown(t)
	pauseTown(t, townRoot, fakeEpoch.Add(time.Hour))

	var pe *doltpause.Error
	if err := h.Start(townRoot); !errors.As(err, &pe) {
		t.Errorf("Start = %v, want *doltpause.Error", err)
	}
	if len(f.started) != 0 || len(f.calls) != 0 {
		t.Errorf("Start while paused started %d processes and ran %v", len(f.started), f.calls)
	}

	err := h.WaitForReady(townRoot, time.Minute)
	if !errors.As(err, &pe) || !strings.HasPrefix(err.Error(), "Dolt paused by deacon until ") {
		t.Errorf("WaitForReady = %v, want the pause message", err)
	}
	if f.slept != 0 {
		t.Errorf("WaitForReady slept %v while paused", f.slept)
	}

	err = h.CheckServerReachable(townRoot)
	if !errors.As(err, &pe) || !strings.Contains(pe.Cause.Error(), "not reachable") {
		t.Errorf("CheckServerReachable = %v, want the pause wrapping not reachable", err)
	}
}

// A lapsed marker is no pause: Start goes ahead and failures read as before.
func TestPausedTown_LapsedMarkerIsIgnored(t *testing.T) {
	t.Parallel()
	f := newFakeHost().townPort(4542)
	h := f.host()
	townRoot := serverModeTown(t)
	pauseTown(t, townRoot, fakeEpoch.Add(-time.Minute))

	var pe *doltpause.Error
	if err := h.CheckServerReachable(townRoot); err == nil || errors.As(err, &pe) {
		t.Errorf("CheckServerReachable = %v, want a plain not-reachable error", err)
	}
	if err := h.WaitForReady(townRoot, time.Second); err == nil || errors.As(err, &pe) {
		t.Errorf("WaitForReady = %v, want a plain timeout", err)
	}
}

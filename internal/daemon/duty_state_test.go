package daemon

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reconcile"
)

// TestDutyStateBuild covers how observations become the duty state record.
func TestDutyStateBuild(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	okLast := now.Add(-90 * time.Second)
	overdueLast := now.Add(-2 * time.Hour)
	// A last good time after now is the unknown verdict with a negative age,
	// and it is the case where the age is not a whole number of seconds.
	futureLast := now.Add(1500 * time.Millisecond)
	obs := []reconcile.Observation{
		{Duty: "backup", LastGood: okLast, Age: 90 * time.Second, Verdict: reconcile.VerdictOK},
		{Duty: "gc", LastGood: overdueLast, Age: 2 * time.Hour, Verdict: reconcile.VerdictOverdue},
		{Duty: "archive", LastGood: futureLast, Age: -1500 * time.Millisecond, Verdict: reconcile.VerdictUnknown},
	}
	limits := map[string]time.Duration{
		"backup":  10 * time.Minute,
		"gc":      30 * time.Minute,
		"archive": 6 * time.Hour,
	}

	t.Run("each observation becomes a record in order", func(t *testing.T) {
		t.Parallel()
		state := newDutyState(now, obs, limits)
		if !state.At.Equal(now) {
			t.Errorf("At = %v, want %v", state.At, now)
		}
		want := []DutyRecord{
			{Name: "backup", Verdict: reconcile.VerdictOK, LastGood: okLast, AgeSeconds: 90, LimitSeconds: 600},
			{Name: "gc", Verdict: reconcile.VerdictOverdue, LastGood: overdueLast, AgeSeconds: 7200, LimitSeconds: 1800},
			{Name: "archive", Verdict: reconcile.VerdictUnknown, LastGood: futureLast, AgeSeconds: -1, LimitSeconds: 21600},
		}
		if len(state.Duties) != len(want) {
			t.Fatalf("got %d duty records, want %d", len(state.Duties), len(want))
		}
		for i := range want {
			if got := state.Duties[i]; got != want[i] {
				t.Errorf("record %d = %+v, want %+v", i, got, want[i])
			}
		}
	})

	t.Run("a duty missing from limits records a zero limit", func(t *testing.T) {
		t.Parallel()
		state := newDutyState(now, []reconcile.Observation{
			{Duty: "town-state", Verdict: reconcile.VerdictUnknown},
		}, limits)
		if got := state.Duties[0].LimitSeconds; got != 0 {
			t.Errorf("LimitSeconds = %d, want 0", got)
		}
	})

	t.Run("no observations records no duties", func(t *testing.T) {
		t.Parallel()
		if got := newDutyState(now, nil, limits); len(got.Duties) != 0 {
			t.Errorf("got %d duty records, want none", len(got.Duties))
		}
	})
}

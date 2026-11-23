package daemon

import (
	"testing"
	"time"
)

// TestTierSweepRunnerTimingNote pins the split an off-host stage's line shows:
// the start lag, the run's tests, and the tail, each rounded to the second; the
// empty string for a result with no start, no finish, or a finish before its
// start; and 0s for a lag or tail a clock skew between the pod and the daemon
// makes negative (gt-hgf1li).
func TestTierSweepRunnerTimingNote(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	const tests = 15*time.Minute + 45*time.Second
	const start = 44 * time.Second
	rows := []struct {
		name                   string
		started, finished, now time.Time
		want                   string
	}{
		{"the normal split", at(start), at(start + tests), at(start + tests + 95*time.Second), " (start lag 44s, tests 15m45s, tail 1m35s)"},
		{"seconds round", at(44600 * time.Millisecond), at(44600*time.Millisecond + tests + 100*time.Millisecond), at(44600*time.Millisecond + tests + 100*time.Millisecond + 94600*time.Millisecond), " (start lag 45s, tests 15m45s, tail 1m35s)"},
		{"no start", time.Time{}, at(tests), at(16 * time.Minute), ""},
		{"no finish", at(start), time.Time{}, at(16 * time.Minute), ""},
		{"finish before start", at(start), at(start - time.Second), at(16 * time.Minute), ""},
		{"a skew makes the lag and the tail negative", at(-3 * time.Second), at(-3*time.Second + tests), at(-3*time.Second + tests - 2*time.Minute), " (start lag 0s, tests 15m45s, tail 0s)"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			got := tierSweepRunnerTimingNote(t0, r.started, r.finished, r.now)
			if got != r.want {
				t.Fatalf("tierSweepRunnerTimingNote(started %s, finished %s, now %s) = %q, want %q",
					r.started, r.finished, r.now, got, r.want)
			}
		})
	}
}

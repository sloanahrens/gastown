package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/spans"
)

// spanClock is the polecat span store's clock, fixed so a test can date the
// attempt it opens.
type spanClock struct{ at time.Time }

func (c spanClock) Now() time.Time { return c.at }

// deadSpanHost is the patrol's host over a town whose spans live where
// CloseDeadSpan writes them.
func deadSpanHost(t *testing.T) (*patrolScanHost, *spans.Store) {
	t.Helper()
	town := t.TempDir()
	return &patrolScanHost{d: &Daemon{config: &Config{TownRoot: town}}}, spans.New(spans.DefaultDir(town), nil)
}

// TestCloseDeadSpanEndsTheAttemptAtTheLastSeenTime: gt sling opens an attempt
// and gt done ends one, so a seat that died without either leaves its bead
// looking unfinished. The tick that confirms the death closes it at the last
// time the seat was seen, marked a lower bound: the time after that is not
// known to be work (gt-dz78b).
func TestCloseDeadSpanEndsTheAttemptAtTheLastSeenTime(t *testing.T) {
	t.Parallel()
	h, store := deadSpanHost(t)
	began := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	if err := spans.New(spans.DefaultDir(h.town()), spanClock{at: began}).Start("gt-abcd1", "gastown/ruby"); err != nil {
		t.Fatalf("start span: %v", err)
	}
	lastSeen := began.Add(7 * time.Minute)

	if err := h.CloseDeadSpan("gt-abcd1", "gastown/ruby", spans.ReasonDead, lastSeen); err != nil {
		t.Fatalf("CloseDeadSpan: %v", err)
	}

	sum, err := store.Summarize("gt-abcd1")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open || !sum.Attempts[0].LowerBound || !sum.LowerBound {
		t.Fatalf("summary = %+v, want one closed lower-bound attempt", sum)
	}
	if got, want := sum.Attempts[0].Secs, lastSeen.Sub(began).Seconds(); got != want {
		t.Errorf("attempt secs = %v, want %v", got, want)
	}
	if sum.Skipped != 0 || sum.Corrupt != 0 {
		t.Errorf("summary skipped %d / corrupt %d events, want none", sum.Skipped, sum.Corrupt)
	}
	raw, err := os.ReadFile(filepath.Join(spans.DefaultDir(h.town()), "gt-abcd1.jsonl"))
	if err != nil {
		t.Fatalf("reading the span log: %v", err)
	}
	end := strings.TrimSpace(strings.Split(strings.TrimSpace(string(raw)), "\n")[1])
	for _, want := range []string{`"event":"end"`, `"reason":"dead"`, `"lower_bound":true`, `"at":"` + lastSeen.Format(time.RFC3339) + `"`} {
		if !strings.Contains(end, want) {
			t.Errorf("end event %s lacks %s", end, want)
		}
	}
}

// TestCloseDeadSpanMovesStaleEvidenceToTheAttemptStart: the last-seen time can
// be a sample from an attempt that already ended. Dated there, the end would
// land before this attempt's start and be skipped on read, leaving the attempt
// open for good, so it is moved to the start (gt-dz78b).
func TestCloseDeadSpanMovesStaleEvidenceToTheAttemptStart(t *testing.T) {
	t.Parallel()
	h, store := deadSpanHost(t)
	began := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	if err := spans.New(spans.DefaultDir(h.town()), spanClock{at: began}).Start("gt-abcd1", "gastown/ruby"); err != nil {
		t.Fatalf("start span: %v", err)
	}

	if err := h.CloseDeadSpan("gt-abcd1", "gastown/ruby", spans.ReasonDead, began.Add(-time.Hour)); err != nil {
		t.Fatalf("CloseDeadSpan: %v", err)
	}

	sum, err := store.Summarize("gt-abcd1")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open {
		t.Fatalf("summary = %+v, want the attempt closed rather than the end skipped", sum)
	}
	if sum.Skipped != 0 || sum.Corrupt != 0 || sum.Secs != 0 || !sum.LowerBound {
		t.Errorf("skipped %d / corrupt %d / secs %v / lower bound %v, want 0/0/0/true",
			sum.Skipped, sum.Corrupt, sum.Secs, sum.LowerBound)
	}
}

// TestCloseDeadSpanWritesNothingWithoutAnOpenAttempt: a seat whose attempt is
// already closed, or whose start never reached the log, has nothing to close,
// so a second confirmation appends no second end.
func TestCloseDeadSpanWritesNothingWithoutAnOpenAttempt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, store *spans.Store)
	}{
		{name: "no log", seed: func(t *testing.T, _ *spans.Store) {}},
		{name: "closed attempt", seed: func(t *testing.T, store *spans.Store) {
			began := time.Now()
			if err := store.Start("gt-abcd1", "gastown/ruby"); err != nil {
				t.Fatal(err)
			}
			if err := store.End("gt-abcd1", "gastown/ruby", spans.ReasonDone, began.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "another seat's attempt", seed: func(t *testing.T, store *spans.Store) {
			if err := store.Start("gt-abcd1", "gastown/onyx"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, store := deadSpanHost(t)
			tc.seed(t, store)
			before := spanLogSize(t, h.town())

			if err := h.CloseDeadSpan("gt-abcd1", "gastown/ruby", spans.ReasonDead, time.Now()); err != nil {
				t.Fatalf("CloseDeadSpan: %v", err)
			}
			if after := spanLogSize(t, h.town()); after != before {
				t.Errorf("span log grew from %d to %d bytes, want nothing written", before, after)
			}
		})
	}
}

// spanLogSize is the size of the bead's span log, 0 when it does not exist.
func spanLogSize(t *testing.T, town string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(spans.DefaultDir(town), "gt-abcd1.jsonl"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("stat span log: %v", err)
	}
	return info.Size()
}

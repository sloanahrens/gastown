package land

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// flakyNoteBeads refuses its first fails AppendNotes calls as a dropped
// connection does, then writes through (gt-hounmt).
type flakyNoteBeads struct {
	*beadsfake.Fake
	fails int
}

func (b *flakyNoteBeads) AppendNotes(id, note string) error {
	if b.fails > 0 {
		b.fails--
		return errors.New("dolt: connection refused")
	}
	return b.Fake.AppendNotes(id, note)
}

// TestRetryWrite: three tries, waiting 2 s then 5 s; a missing bead, a context
// canceled before the failure, or one canceled in the wait stops the retries.
func TestRetryWrite(t *testing.T) {
	t.Parallel()
	drop := errors.New("dolt: connection refused")
	for _, tc := range []struct {
		name         string
		tries        []error // what each try returns, in order; past the end, nil
		cancelBefore bool
		cancelWait   bool
		want         error
		calls, waits int
	}{
		{"success on the third try", []error{drop, drop}, false, false, nil, 3, 2},
		{"three failures return the last error", []error{drop, drop, drop}, false, false, drop, 3, 2},
		{"a missing bead is not retried", []error{beads.ErrNotFound}, false, false, beads.ErrNotFound, 1, 0},
		{"a context canceled before the failure does not wait", []error{drop}, true, false, drop, 1, 0},
		{"a context canceled in the wait does not try again", []error{drop}, false, true, drop, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			var waits []time.Duration
			sleep := func(d time.Duration) {
				waits = append(waits, d)
				if tc.cancelWait {
					cancel()
				}
			}
			calls := 0
			err := retryWrite(ctx, sleep, func() error {
				i := calls
				calls++
				if i < len(tc.tries) {
					return tc.tries[i]
				}
				return nil
			})
			if !errors.Is(err, tc.want) || calls != tc.calls {
				t.Errorf("retryWrite = %v after %d tries; want %v after %d", err, calls, tc.want, tc.calls)
			}
			if want := []time.Duration{2 * time.Second, 5 * time.Second}[:tc.waits]; !slices.Equal(waits, want) {
				t.Errorf("waits = %v; want %v", waits, want)
			}
		})
	}
}

// pastDeadline is a context whose deadline has passed, though its Err has not.
type pastDeadline struct{ context.Context }

func (pastDeadline) Deadline() (time.Time, bool) { return time.Unix(0, 0), true }

// TestRetryWriteStopsAtTheDeadline: a wait past the deadline is not taken (gt-y82k93).
func TestRetryWriteStopsAtTheDeadline(t *testing.T) {
	t.Parallel()
	calls, paused := 0, false
	err := retryWrite(pastDeadline{context.Background()}, func(time.Duration) { paused = true }, func() error {
		calls++
		return errors.New("dolt: connection refused")
	})
	if err == nil || calls != 1 || paused {
		t.Errorf("retryWrite = %v after %d tries (paused %v); want the error after 1 try", err, calls, paused)
	}
}

// TestRejectRetriesTheNote: a note that fails twice is recorded with no
// RecordErr; one that fails three times is reported on it (gt-hounmt).
func TestRejectRetriesTheNote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		fails    int
		recorded bool
	}{
		{"a note that fails twice is recorded", 2, true},
		{"a note that fails three times is reported", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l := f.lander()
			l.Beads = &flakyNoteBeads{Fake: f.bd, fails: tc.fails}
			l.sleep = func(time.Duration) {}
			rej := &Rejection{Kind: RejectGate, Rework: true, Reason: "r"}
			if err := l.reject(context.Background(), f.bead(), f.work, rej, nil); err != rej {
				t.Fatalf("reject = %v; want the rejection", err)
			}
			_, noted := ParseRejectionNote(f.bead().Notes)
			if (rej.RecordErr == nil) != tc.recorded || noted != tc.recorded {
				t.Errorf("RecordErr = %v, noted = %v; want recorded = %v", rej.RecordErr, noted, tc.recorded)
			}
		})
	}
}

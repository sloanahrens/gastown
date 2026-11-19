package land

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// queueAt is a queue in a directory the queue accepts, or with blocked a queue
// whose directory is a file, so it cannot be made (gt-r2rmz6). t.TempDir is 0777
// under the CI runner's open umask, which the queue refuses.
func queueAt(t *testing.T, blocked bool) RecordQueue {
	t.Helper()
	dir := t.TempDir()
	if blocked {
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return RecordQueue{Path: filepath.Join(blocker, "landing-records.jsonl")}
	}
	//testpolicy:allow no-exec-files — a directory, not a file: its owner search bit lets the queue reach its file
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return RecordQueue{Path: filepath.Join(dir, "landing-records.jsonl")}
}

// TestRejectQueuesTheNote: a note that fails all three tries is queued, one
// that is written is not, and a queue that also fails is named (gt-r2rmz6).
func TestRejectQueuesTheNote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		fails   int
		blocked bool // the queue's directory cannot be made
		queued  int
	}{
		{"a note that fails all three tries is queued", 3, false, 1},
		{"a note that is written queues nothing", 0, false, 0},
		{"a note that cannot be queued either names both", 3, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l := f.lander()
			l.Beads = &flakyNoteBeads{Fake: f.bd, fails: tc.fails}
			l.sleep = func(time.Duration) {}
			q := queueAt(t, tc.blocked)
			l.Records = &q
			rej := &Rejection{Kind: RejectGate, Rework: true, Reason: "r"}
			if err := l.reject(context.Background(), f.bead(), f.work, rej, nil); err != rej {
				t.Fatalf("reject = %v; want the rejection", err)
			}
			if (rej.RecordErr != nil) != (tc.fails > 0) {
				t.Errorf("RecordErr = %v; want it set = %v", rej.RecordErr, tc.fails > 0)
			}
			if tc.blocked {
				if !strings.Contains(rej.RecordErr.Error(), "queueing it for a later pass failed") {
					t.Errorf("RecordErr = %v; want the failed queue named", rej.RecordErr)
				}
				return
			}
			got, err := q.Pending()
			if err != nil || len(got) != tc.queued {
				t.Fatalf("Pending = %+v, %v; want %d queued", got, err, tc.queued)
			}
			if tc.queued == 1 {
				if n, ok := ParseRejectionNote(got[0].Text); !ok || n.Reason != "r" || n.Head != f.work.Head || got[0].Kind != RecordKindNotes {
					t.Errorf("queued %+v; want the notes block for head %s", got[0], f.work.Head)
				}
			}
		})
	}
}

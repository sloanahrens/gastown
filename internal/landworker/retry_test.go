package landworker

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

type flakyBeads struct {
	RetryInner
	fails int
	err   error
	calls int
}

func (f *flakyBeads) AppendNotes(id, note string) error {
	f.calls++
	if f.calls <= f.fails {
		return f.err
	}
	return f.RetryInner.AppendNotes(id, note)
}

func TestRetryBeadsRetriesSerializationFailures(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New(beadsfake.WithPrefix("gt"))
	fake.Seed(beads.Issue{ID: "gt-a", Title: "a", Type: "task"})
	inner := &flakyBeads{RetryInner: fake, fails: 2, err: errors.New("bd update: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction")}
	var waits []time.Duration
	r := RetryBeads{Inner: inner, Sleep: func(d time.Duration) { waits = append(waits, d) }}
	if err := r.AppendNotes("gt-a", "note"); err != nil {
		t.Fatalf("AppendNotes: %v", err)
	}
	if inner.calls != 3 || len(waits) != 2 {
		t.Fatalf("calls %d waits %v; want 3 calls and 2 waits", inner.calls, waits)
	}
	for _, w := range waits {
		if w < 100*time.Millisecond || w > 2*time.Second {
			t.Fatalf("wait %s outside [100ms, 2s]", w)
		}
	}
}

func TestRetryBeadsGivesUpAfterTries(t *testing.T) {
	t.Parallel()
	inner := &flakyBeads{RetryInner: beadsfake.New(), fails: 99, err: errors.New("Error 1213: serialization failure")}
	r := RetryBeads{Inner: inner, Tries: 3, Sleep: func(time.Duration) {}}
	if err := r.AppendNotes("gt-a", "n"); err == nil || inner.calls != 3 {
		t.Fatalf("err %v calls %d; want the error after 3 calls", err, inner.calls)
	}
}

func TestRetryBeadsDoesNotRetryOtherErrors(t *testing.T) {
	t.Parallel()
	inner := &flakyBeads{RetryInner: beadsfake.New(), fails: 99, err: errors.New("issue not found")}
	r := RetryBeads{Inner: inner, Sleep: func(time.Duration) { t.Fatal("slept on a non-retryable error") }}
	if err := r.AppendNotes("gt-a", "n"); err == nil || inner.calls != 1 {
		t.Fatalf("err %v calls %d; want one call", err, inner.calls)
	}
}

func TestRetryDelayBounds(t *testing.T) {
	t.Parallel()
	for attempt := 0; attempt < 10; attempt++ {
		for i := 0; i < 50; i++ {
			if d := retryDelay(attempt); d < 100*time.Millisecond || d > 2*time.Second {
				t.Fatalf("retryDelay(%d) = %s", attempt, d)
			}
		}
	}
}

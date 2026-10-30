package land

import (
	"errors"
	"testing"
)

// fakeFirstSight is one recorder in the composite: it reports what it wrote
// and what it failed with, and counts the calls.
type fakeFirstSight struct {
	wrote bool
	err   error
	calls int
	seen  []string
}

func (f *fakeFirstSight) Record(w Work) (bool, error) {
	f.calls++
	f.seen = append(f.seen, w.BeadID)
	return f.wrote, f.err
}

// TestFirstSightCompositeRunsEveryRecorder: one recorder's failure is the
// error the worker logs, but it never costs the other its record.
func TestFirstSightCompositeRunsEveryRecorder(t *testing.T) {
	t.Parallel()
	dead := &fakeFirstSight{err: errors.New("first-sight log is unreadable")}
	second := &fakeFirstSight{wrote: true}
	third := &fakeFirstSight{err: errors.New("done log is unreadable")}
	c := &FirstSightComposite{Recorders: []FirstSight{dead, second, third}}

	w := doneWork()
	wrote, err := c.Record(w)
	if err == nil || err.Error() != "first-sight log is unreadable" {
		t.Fatalf("Record error = %v, want the first recorder's error", err)
	}
	if !wrote {
		t.Fatal("Record = false; the second recorder wrote")
	}
	for i, rec := range []*fakeFirstSight{dead, second, third} {
		if rec.calls != 1 {
			t.Errorf("recorder %d called %d times, want 1", i, rec.calls)
		}
	}
}

// A nil hook is skipped, and so is an interface holding a nil pointer: a
// recorder field that was never set must not panic the landing pass.
func TestFirstSightCompositeSkipsANilRecorder(t *testing.T) {
	t.Parallel()
	var typedNil *DoneEventRecorder
	live := &fakeFirstSight{wrote: true}
	c := &FirstSightComposite{Recorders: []FirstSight{nil, typedNil, live}}

	wrote, err := c.Record(doneWork())
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !wrote || live.calls != 1 {
		t.Fatalf("Record = %v with %d live calls, want true with one", wrote, live.calls)
	}
}

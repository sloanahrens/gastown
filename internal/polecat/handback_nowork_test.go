package polecat

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestHandBackNoWorkWritesNoLabel: a seat that stopped with no commit of its own is not a size
// finding, so the bead goes back open and unassigned with no label and a comment that says so
// and names the elapsed time (gt-36pe8o).
func TestHandBackNoWorkWritesNoLabel(t *testing.T) {
	t.Parallel()
	w := &fakeBeadWriter{}
	if err := HandBackNoWork(context.Background(), w, "gt-abc", stoppedSeat, 20*time.Minute+400*time.Millisecond, "The seat's stream was cut."); err != nil {
		t.Fatalf("HandBackNoWork: %v", err)
	}
	if len(w.calls) != 3 {
		t.Fatalf("got %d writes, want 3 (status, assignee, comment and no label): %q", len(w.calls), w.calls)
	}
	if want := []string{"status gt-abc open", "assignee gt-abc"}; !reflect.DeepEqual(w.calls[:2], want) {
		t.Errorf("first two writes = %q, want %q", w.calls[:2], want)
	}
	comment := w.calls[2]
	for _, part := range []string{"20m0s", "with no commit on its branch", "not a size finding", "do not split the bead", "The seat's stream was cut."} {
		if !strings.Contains(comment, part) {
			t.Errorf("comment %q lacks %q", comment, part)
		}
	}
	for _, c := range w.calls {
		if strings.HasPrefix(c, "label ") || strings.Contains(c, NeedsSplitLabel) && !strings.Contains(c, "not a size finding") {
			t.Errorf("write %q puts a label on the bead", c)
		}
	}
}

// TestHandBackNoWorkSkipsAndFails: a closed bead and one assigned to another polecat are left
// alone, and a failing write stops the hand-back wrapped with its step (gt-36pe8o).
func TestHandBackNoWorkSkipsAndFails(t *testing.T) {
	t.Parallel()
	closed := &fakeBeadWriter{status: "closed"}
	if err := HandBackNoWork(context.Background(), closed, "gt-abc", stoppedSeat, time.Minute, ""); !errors.Is(err, ErrHandBackSkipped) || len(closed.calls) != 0 {
		t.Errorf("closed bead: err %v, writes %q, want skipped and none", err, closed.calls)
	}
	other := &fakeBeadWriter{status: "hooked", assignee: "gastown/polecats/capable"}
	if err := HandBackNoWork(context.Background(), other, "gt-abc", stoppedSeat, time.Minute, ""); !errors.Is(err, ErrHandBackSkipped) || len(other.calls) != 0 {
		t.Errorf("bead of another polecat: err %v, writes %q, want skipped and none", err, other.calls)
	}
	for i, op := range []string{"status", "assignee", "comment"} {
		w := &fakeBeadWriter{failOn: op}
		err := HandBackNoWork(context.Background(), w, "gt-abc", stoppedSeat, time.Minute, "")
		if err == nil || !strings.Contains(err.Error(), op+" refused") {
			t.Fatalf("failing %s: err = %v, want it to wrap %q", op, err, op+" refused")
		}
		if len(w.calls) != i+1 {
			t.Errorf("failing %s: writes = %q, want exactly %d", op, w.calls, i+1)
		}
	}
}

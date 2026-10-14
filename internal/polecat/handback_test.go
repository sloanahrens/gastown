package polecat

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeBeadWriter records each write as "op id arg" and fails the op named in
// failOn, so a test reads the order and the stopping point straight off calls.
type fakeBeadWriter struct {
	calls  []string
	failOn string
	// status and assignee are what State reports; an empty status reads as
	// the stopped seat's hooked bead.
	status, assignee string
	stateErr         error
}

// stoppedSeat is the bead assignee of the polecat the tests stop.
const stoppedSeat = "gastown/polecats/nux"

func (f *fakeBeadWriter) State(context.Context, string) (string, string, error) {
	if f.stateErr != nil {
		return "", "", f.stateErr
	}
	if f.status == "" {
		return "hooked", stoppedSeat, nil
	}
	return f.status, f.assignee, nil
}

func (f *fakeBeadWriter) record(op, id, arg string) error {
	f.calls = append(f.calls, strings.TrimSpace(op+" "+id+" "+arg))
	if op == f.failOn {
		return errors.New(op + " refused")
	}
	return nil
}

func (f *fakeBeadWriter) SetStatus(_ context.Context, id, status string) error {
	return f.record("status", id, status)
}
func (f *fakeBeadWriter) ClearAssignee(_ context.Context, id string) error {
	return f.record("assignee", id, "")
}
func (f *fakeBeadWriter) AddLabel(_ context.Context, id, label string) error {
	return f.record("label", id, label)
}
func (f *fakeBeadWriter) Comment(_ context.Context, id, text string) error {
	return f.record("comment", id, text)
}

func TestHandBack_OpensClearsLabelsAndComments(t *testing.T) {
	t.Parallel()
	w := &fakeBeadWriter{}
	if err := HandBack(context.Background(), w, "gt-abc", stoppedSeat, "polecat/nux/gt-abc+x1", 20*time.Minute+400*time.Millisecond); err != nil {
		t.Fatalf("HandBack: %v", err)
	}
	if len(w.calls) != 4 {
		t.Fatalf("got %d writes, want 4: %q", len(w.calls), w.calls)
	}
	want := []string{"status gt-abc open", "assignee gt-abc", "label gt-abc needs-split"}
	if !reflect.DeepEqual(w.calls[:3], want) {
		t.Errorf("first three writes = %q, want %q", w.calls[:3], want)
	}
	comment := w.calls[3]
	if !strings.HasPrefix(comment, "comment gt-abc ") {
		t.Errorf("last write = %q, want a comment on gt-abc", comment)
	}
	for _, part := range []string{"polecat/nux/gt-abc+x1", "20m0s"} {
		if !strings.Contains(comment, part) {
			t.Errorf("comment %q does not name %q", comment, part)
		}
	}
}

func TestHandBackNoted_AppendsTheNoteToTheComment(t *testing.T) {
	t.Parallel()
	w := &fakeBeadWriter{}
	if err := HandBackNoted(context.Background(), w, "gt-abc", stoppedSeat, "b1", time.Minute, "Worktree not checkpointed."); err != nil {
		t.Fatalf("HandBackNoted: %v", err)
	}
	comment := w.calls[len(w.calls)-1]
	if !strings.HasSuffix(comment, " Worktree not checkpointed.") || !strings.Contains(comment, "work kept on branch b1") {
		t.Errorf("comment = %q, want the standard text then the note", comment)
	}
}

// A repeated hand-back reads the bead first: one that was closed, or slung to
// another polecat after a partial first hand-back, is left alone, while the
// half-released bead (open, unassigned) is finished.
func TestHandBack_ReadsTheBeadBeforeWriting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, status, assignee string
		stateErr               error
		wantSkip               bool
		wantCalls              int
	}{
		{name: "hooked to the stopped seat", status: "hooked", assignee: stoppedSeat, wantCalls: 4},
		{name: "half released: open and unassigned", status: "open", assignee: "", wantCalls: 4},
		{name: "re-slung to another polecat", status: "hooked", assignee: "gastown/polecats/capable", wantSkip: true},
		{name: "re-slung and in progress", status: "in_progress", assignee: "gastown/polecats/capable", wantSkip: true},
		{name: "closed and unassigned", status: "closed", assignee: "", wantSkip: true},
		{name: "closed by the stopped seat", status: "closed", assignee: stoppedSeat, wantSkip: true},
		{name: "tombstone", status: "tombstone", assignee: "", wantSkip: true},
		{name: "unreadable", stateErr: errors.New("bd down")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := &fakeBeadWriter{status: tc.status, assignee: tc.assignee, stateErr: tc.stateErr}
			err := HandBack(context.Background(), w, "gt-abc", stoppedSeat, "b", time.Minute)
			switch {
			case tc.wantSkip:
				if !errors.Is(err, ErrHandBackSkipped) {
					t.Errorf("err = %v, want ErrHandBackSkipped", err)
				}
			case tc.stateErr != nil:
				if !errors.Is(err, tc.stateErr) || errors.Is(err, ErrHandBackSkipped) {
					t.Errorf("err = %v, want the read error and not a skip", err)
				}
			case err != nil:
				t.Errorf("err = %v, want the hand-back to run", err)
			}
			if len(w.calls) != tc.wantCalls {
				t.Errorf("writes = %q, want %d", w.calls, tc.wantCalls)
			}
		})
	}
}

func TestHandBack_StopsOnFirstError(t *testing.T) {
	t.Parallel()
	order := []string{"status", "assignee", "label", "comment"}
	for i, op := range order {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			w := &fakeBeadWriter{failOn: op}
			err := HandBack(context.Background(), w, "gt-abc", stoppedSeat, "b", time.Minute)
			if err == nil || !strings.Contains(err.Error(), op+" refused") {
				t.Fatalf("err = %v, want it to wrap %q", err, op+" refused")
			}
			if len(w.calls) != i+1 {
				t.Errorf("writes = %q, want exactly %d (none after the failed %s)", w.calls, i+1, op)
			}
		})
	}
}

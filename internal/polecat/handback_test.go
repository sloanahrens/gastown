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
	if err := HandBack(context.Background(), w, "gt-abc", "polecat/nux/gt-abc+x1", 20*time.Minute+400*time.Millisecond); err != nil {
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

func TestHandBack_StopsOnFirstError(t *testing.T) {
	t.Parallel()
	order := []string{"status", "assignee", "label", "comment"}
	for i, op := range order {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			w := &fakeBeadWriter{failOn: op}
			err := HandBack(context.Background(), w, "gt-abc", "b", time.Minute)
			if err == nil || !strings.Contains(err.Error(), op+" refused") {
				t.Fatalf("err = %v, want it to wrap %q", err, op+" refused")
			}
			if len(w.calls) != i+1 {
				t.Errorf("writes = %q, want exactly %d (none after the failed %s)", w.calls, i+1, op)
			}
		})
	}
}

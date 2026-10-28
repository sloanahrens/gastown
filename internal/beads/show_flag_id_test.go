package beads

import (
	"errors"
	"slices"
	"testing"
)

// flagShapedIDs are ids from an untrusted source (a log line, a bead field)
// that bd would read as flags rather than as the name of a bead.
var flagShapedIDs = []string{"-x", "-h", "--help", "--json", "--"}

// TestShowRefusesAFlagShapedID: Show stops at the wrapper, so bd never sees an
// id that would change its behaviour, and the refusal is its own error rather
// than the ErrNotFound a missing bead answers (gt-ecfaa).
func TestShowRefusesAFlagShapedID(t *testing.T) {
	t.Parallel()
	for _, id := range flagShapedIDs {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(nil)
			got, err := newRecordedBeads(t.TempDir(), r).Show(id)
			if !errors.Is(err, errFlagShapedID) {
				t.Fatalf("Show(%q) = %+v, %v, want errFlagShapedID", id, got, err)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("Show(%q) = %v, want a refusal, not ErrNotFound", id, err)
			}
			if calls := r.argvs(); len(calls) != 0 {
				t.Errorf("Show(%q) ran bd: %v", id, calls)
			}
		})
	}
}

// TestShowMultipleRefusesAFlagShapedID: one flag-shaped id refuses the whole
// call before any store is reached, rather than being silently left out the way
// an absent id is (gt-ecfaa).
func TestShowMultipleRefusesAFlagShapedID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ids  []string
	}{
		{"only a flag", []string{"--json"}},
		{"a real id before a flag", []string{"gt-a", "-x"}},
		{"a flag before a real id", []string{"-h", "gt-a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ids := tc.ids
			r := newRecorder(nil)
			got, err := newRecordedBeads(t.TempDir(), r).ShowMultiple(ids)
			if !errors.Is(err, errFlagShapedID) {
				t.Fatalf("ShowMultiple(%v) = %v, %v, want errFlagShapedID", ids, got, err)
			}
			if len(got) != 0 {
				t.Errorf("ShowMultiple(%v) = %v, want no issues", ids, got)
			}
			if calls := r.argvs(); len(calls) != 0 {
				t.Errorf("ShowMultiple(%v) ran bd: %v", ids, calls)
			}
		})
	}
}

// TestVerifyBatchClosedRefusesAFlagShapedID: the batch close re-read reaches
// bd's show through showMultipleLocal without passing ShowMultiple, so the
// builder refuses the id itself; the wrapper's check never sees this call
// (gt-lw0mh).
func TestVerifyBatchClosedRefusesAFlagShapedID(t *testing.T) {
	t.Parallel()
	for _, id := range flagShapedIDs {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(nil)
			err := newRecordedBeads(t.TempDir(), r).verifyBatchClosed([]string{"gt-a", id})
			if !errors.Is(err, errFlagShapedID) {
				t.Fatalf("verifyBatchClosed(gt-a, %q) = %v, want errFlagShapedID", id, err)
			}
			if calls := r.argvs(); len(calls) != 0 {
				t.Errorf("verifyBatchClosed(gt-a, %q) ran bd: %v", id, calls)
			}
		})
	}
}

// TestShowReadsAnIDThatIsNotFlagShaped: the refusal covers a leading dash only,
// so an ordinary id still reaches bd and comes back as its issue (gt-ecfaa).
func TestShowReadsAnIDThatIsNotFlagShaped(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply {
		return reply{stdout: `[{"id":"gt-a","title":"a"}]`}
	})
	got, err := newRecordedBeads(t.TempDir(), r).Show("gt-a")
	if err != nil {
		t.Fatalf("Show(gt-a) = %v", err)
	}
	if got == nil || got.ID != "gt-a" {
		t.Fatalf("Show(gt-a) = %+v, want gt-a", got)
	}
	calls := r.calls()
	if len(calls) != 1 || !slices.Contains(calls[0].args, "gt-a") {
		t.Errorf("bd calls = %v, want one show of gt-a", r.argvs())
	}
}

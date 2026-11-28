package land

import (
	"errors"
	"os"
	"testing"
)

// failingVerdicts fails its write through the store's own seam (gt-c789n9).
func failingVerdicts(t *testing.T, err error) *VerdictStore {
	return &VerdictStore{TownRoot: t.TempDir(), Rig: "gastown",
		writeFile: func(string, []byte, os.FileMode) error { return err }}
}

// reviewStored reviews at head "bbb" for bead through r, whose om approves.
func reviewStored(t *testing.T, r OMReviewer, bead string) (Verdict, error) {
	var argv []string
	r.run = fakeOM(t, isolatedApprove, 0, nil, &argv)
	return r.reviewOnce(t.Context(), ReviewInput{Dir: "/wt", Base: "aaa", Head: "bbb", BeadID: bead, Repo: baseRepo{}})
}

// TestStoreFailed: a verdict the store could not write is reported to the
// wired func once, and the review still returns its verdict (gt-c789n9).
func TestStoreFailed(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("no room")
	t.Run("a failed write reports once and the verdict stands", func(t *testing.T) {
		t.Parallel()
		var calls int
		var bead, head string
		var got error
		r := OMReviewer{OutDir: t.TempDir(), Verdicts: failingVerdicts(t, writeErr),
			StoreFailed: func(b, h string, err error) { calls++; bead, head, got = b, h, err }}
		v, err := reviewStored(t, r, "gt-c789n9")
		if err != nil || v.Verdict != VerdictApprove {
			t.Fatalf("verdict = %q err = %v, want %q on a failed write", v.Verdict, err, VerdictApprove)
		}
		if calls != 1 || bead != "gt-c789n9" || head != "bbb" || !errors.Is(got, writeErr) {
			t.Fatalf("StoreFailed = %d call(s), bead %q, head %q, err %v; want gt-c789n9, bbb, %v", calls, bead, head, got, writeErr)
		}
	})
	t.Run("a successful write calls nothing", func(t *testing.T) {
		t.Parallel()
		var calls int
		r := OMReviewer{OutDir: t.TempDir(), Verdicts: &VerdictStore{TownRoot: t.TempDir(), Rig: "gastown"},
			StoreFailed: func(string, string, error) { calls++ }}
		if _, err := reviewStored(t, r, "gt-c789n9"); err != nil {
			t.Fatal(err)
		}
		if calls != 0 {
			t.Fatalf("StoreFailed called %d time(s) for a stored verdict, want none", calls)
		}
	})
	t.Run("a nil StoreFailed with a failed write returns the verdict", func(t *testing.T) {
		t.Parallel()
		r := OMReviewer{OutDir: t.TempDir(), Verdicts: failingVerdicts(t, writeErr)}
		if v, err := reviewStored(t, r, "gt-c789n9"); err != nil || v.Verdict != VerdictApprove {
			t.Fatalf("verdict = %q err = %v, want %q with a failed write and no StoreFailed", v.Verdict, err, VerdictApprove)
		}
	})
	t.Run("an isolated copy reports through the same func", func(t *testing.T) {
		t.Parallel()
		var calls int
		r := OMReviewer{OutDir: t.TempDir(), StoreFailed: func(string, string, error) { calls++ }}
		c, ok := r.Isolated(t.TempDir()).(OMReviewer)
		if !ok {
			t.Fatalf("%T is not an OMReviewer", c)
		}
		// Isolated drops the store; the row gives the copy one to drive the write.
		c.Verdicts = failingVerdicts(t, writeErr)
		if _, err := reviewStored(t, c, "gt-c789n9"); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("the copy's failed write called StoreFailed %d time(s), want once", calls)
		}
	})
}

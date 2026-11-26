package land

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// storedOnlyUntouched asserts a stored-only pass left the landing alone: no
// review, no gate, no bead write, no record, and the target unmoved. calls
// counts the reviews the pass's reviewer ran.
func storedOnlyUntouched(t *testing.T, f *landFixture, calls func() int, writes int) {
	t.Helper()
	if n := calls(); n != 0 {
		t.Errorf("om ran %d time(s) on a stored-only pass; want 0", n)
	}
	if n := len(f.gate.dirs); n != 0 {
		t.Errorf("gate ran %d time(s) on a stored-only pass; want 0", n)
	}
	if after := aheadBeadWrites(t, f.bd); after != writes {
		t.Errorf("the bead journal moved from %d to %d write(s); a stored-only pass writes no bead", writes, after)
	}
	if got := f.landingLines(); len(got) != 0 {
		t.Errorf("landings file written on a stored-only pass: %q", got)
	}
	if got := f.originMain(); got != f.base {
		t.Errorf("origin/main moved to %s; want the pass to leave it at %s", got, f.base)
	}
}

// storedOnlyHasRecord reports whether the store keeps a record for the fixture's
// bead, read through a fresh store so a row's read seam cannot hide it.
func storedOnlyHasRecord(t *testing.T, f *landFixture) bool {
	t.Helper()
	_, ok, err := TownAheadStore(f.town, "gastown").Get(f.work.BeadID)
	if err != nil {
		t.Fatalf("reading the stored record = %v; want a readable store", err)
	}
	return ok
}

// storedOnlyPut stores rec for the fixture's bead.
func storedOnlyPut(t *testing.T, store *AheadStore, rec AheadRecord) {
	t.Helper()
	if err := store.Put(rec); err != nil {
		t.Fatalf("Put error = %v; want nil", err)
	}
}

// storedOnlyRejection is a stored request_changes with one finding.
func storedOnlyRejection() Verdict {
	return Verdict{Verdict: VerdictRequestChanges, Score: 0.3, Summary: "b.txt is wrong.",
		Findings: []Finding{{ID: "abc123", Severity: "major", Path: "b.txt", Line: 1, Title: "wrong"}}}
}

// storedOnlyConflict is the record an ahead merge that conflicted leaves: no
// verdict, because the merge never reached om.
func storedOnlyConflict(key ReviewKey, base string, reviewedAt time.Time) AheadRecord {
	rec := reuseAskRecord(key, base, reviewedAt)
	rec.Outcome, rec.Verdict = aheadOutcomeConflict, Verdict{}
	return rec
}

// TestVerdictReuseStoredOnlyRejects: a stored request_changes rejects through the
// unchanged reject path — kind review, one MERGE REJECTION note carrying its
// finding, no om, no gate (gt-6r4exl). Turning it into the sentinel turns this red.
func TestVerdictReuseStoredOnlyRejects(t *testing.T) {
	t.Parallel()
	f, l, store, key := reuseLandSetup(t)
	rec := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
	rec.Verdict = storedOnlyRejection()
	storedOnlyPut(t, store, rec)
	f.work.StoredOnly = true
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelRework)
	if !rej.Rework || rej.ReviewScore != 0.3 || len(rej.Findings) != 1 {
		t.Errorf("rejection = %+v; want the stored score 0.3 and its one finding", rej)
	}
	if !strings.Contains(f.bead().Notes, "abc123") {
		t.Errorf("MERGE REJECTION note does not carry the stored finding:\n%s", f.bead().Notes)
	}
	if n := len(f.review.calls); n != 0 {
		t.Errorf("om ran %d time(s) on a stored request_changes; want 0", n)
	}
	if n := len(f.gate.dirs); n != 0 {
		t.Errorf("gate ran %d time(s) on a refused stored verdict; want 0", n)
	}
}

// TestVerdictReuseStoredOnlyApproveIsSentinel: a stored approve is consumed only
// at the bead's own landing, so the pass ends with ErrNoStoredVerdict and keeps it.
func TestVerdictReuseStoredOnlyApproveIsSentinel(t *testing.T) {
	t.Parallel()
	f, l, store, key := reuseLandSetup(t)
	storedOnlyPut(t, store, reuseAskRecord(key, f.base, l.now().Add(-time.Minute)))
	f.work.StoredOnly = true
	writes := aheadBeadWrites(t, f.bd)
	_, err := l.Land(context.Background(), f.work)
	if !errors.Is(err, ErrNoStoredVerdict) {
		t.Fatalf("Land error = %v; want ErrNoStoredVerdict", err)
	}
	if !storedOnlyHasRecord(t, f) {
		t.Error("the stored approve was not kept; the bead's own landing needs it")
	}
	storedOnlyUntouched(t, f, func() int { return len(f.review.calls) }, writes)
}

// TestVerdictReuseStoredOnlyWaivedIsSentinel: a head the overseer reviewed ends
// the pass before the reuse lookup, so a stored rejection cannot refuse a head a
// waiver would have let through (gt-6r4exl). Reading the waiver after the reuse
// leaves the bead rejected and turns this red.
func TestVerdictReuseStoredOnlyWaivedIsSentinel(t *testing.T) {
	t.Parallel()
	f, l, store, key := reuseLandSetup(t)
	rec := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
	rec.Verdict = storedOnlyRejection()
	storedOnlyPut(t, store, rec)
	if err := f.bd.AppendNotes(f.work.BeadID, FormatOverseerReviewedNote(f.work.Head)); err != nil {
		t.Fatal(err)
	}
	if err := f.bd.Update(f.work.BeadID, beads.UpdateOptions{AddLabels: []string{LabelOverseerReviewed}}); err != nil {
		t.Fatal(err)
	}
	f.work.StoredOnly = true
	writes := aheadBeadWrites(t, f.bd)
	_, err := l.Land(context.Background(), f.work)
	if !errors.Is(err, ErrNoStoredVerdict) {
		t.Fatalf("Land error = %v; want ErrNoStoredVerdict", err)
	}
	if !storedOnlyHasRecord(t, f) {
		t.Error("the stored record was not kept; a waived head consumes nothing")
	}
	storedOnlyUntouched(t, f, func() int { return len(f.review.calls) }, writes)
}

// TestVerdictReuseStoredOnlyNilAhead: with no store there is nothing to consume,
// so the pass ends with ErrNoStoredVerdict before the reuse lookup dereferences
// the nil store (gt-6r4exl).
func TestVerdictReuseStoredOnlyNilAhead(t *testing.T) {
	t.Parallel()
	f, l, _, _ := reuseLandSetup(t)
	l.Ahead = nil
	f.work.StoredOnly = true
	writes := aheadBeadWrites(t, f.bd)
	_, err := l.Land(context.Background(), f.work)
	if !errors.Is(err, ErrNoStoredVerdict) {
		t.Fatalf("Land error = %v; want ErrNoStoredVerdict", err)
	}
	storedOnlyUntouched(t, f, func() int { return len(f.review.calls) }, writes)
}

// TestVerdictReuseStoredOnlyMissDeletes: a record that definitely does not answer
// for this landing, or no record at all, ends the pass with ErrNoStoredVerdict and
// leaves nothing stored (gt-6r4exl). Keeping a mismatched record turns the rows red.
func TestVerdictReuseStoredOnlyMissDeletes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// seed stores a record for the work first; edit changes it.
		seed bool
		edit func(rec *AheadRecord)
	}{
		{name: "no record"},
		{name: "another head", seed: true, edit: func(rec *AheadRecord) { rec.Head = strings.Repeat("cd", 20) }},
		{name: "an expired record", seed: true,
			edit: func(rec *AheadRecord) { rec.ReviewedAt = rec.ReviewedAt.Add(-2 * time.Hour) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, store, key := reuseLandSetup(t)
			if tc.seed {
				rec := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
				tc.edit(&rec)
				storedOnlyPut(t, store, rec)
			}
			f.work.StoredOnly = true
			writes := aheadBeadWrites(t, f.bd)
			_, err := l.Land(context.Background(), f.work)
			if !errors.Is(err, ErrNoStoredVerdict) {
				t.Fatalf("Land error = %v; want ErrNoStoredVerdict", err)
			}
			if storedOnlyHasRecord(t, f) {
				t.Error("a record that cannot be reused was kept; no later pass can consume it")
			}
			storedOnlyUntouched(t, f, func() int { return len(f.review.calls) }, writes)
		})
	}
}

// TestVerdictReuseStoredOnlyErrorKeeps: an answer reuseAhead could not give ends
// the pass with ErrNoStoredVerdict and KEEPS the record: the pass runs between
// every landing, so a transient failure must not throw a verdict away (gt-6r4exl).
// Deleting on an error turns this red. A record that will not read fails earlier,
// in the conflict check before the merge, as infrastructure (gt-1yvte7); it is
// still kept.
func TestVerdictReuseStoredOnlyErrorKeeps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(l *Lander)
		// reviewer replaces the lander's reviewer when set, and counts the
		// reviews it ran; nil keeps the setup's (f.review).
		reviewer func() (Reviewer, func() int)
		// wantInfra: the pass fails as infrastructure rather than with the sentinel.
		wantInfra bool
	}{
		{
			name: "a record that will not read",
			edit: func(l *Lander) {
				l.Ahead.readFile = func(string) ([]byte, error) { return nil, errors.New("record unreadable") }
			},
			wantInfra: true,
		},
		{
			name: "a review key that cannot be computed",
			edit: func(*Lander) {},
			reviewer: func() (Reviewer, func() int) {
				// Not a VersionProber, so the key cannot name om's version.
				rv := &fakeReviewer{fn: func(string) (Verdict, error) { return Verdict{Verdict: VerdictApprove, Score: 0.9}, nil }}
				return rv, func() int { return len(rv.calls) }
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, store, key := reuseLandSetup(t)
			storedOnlyPut(t, store, reuseAskRecord(key, f.base, l.now().Add(-time.Minute)))
			calls := func() int { return len(f.review.calls) }
			if tc.reviewer != nil {
				var r Reviewer
				r, calls = tc.reviewer()
				l.Reviewer = r
			}
			tc.edit(l)
			f.work.StoredOnly = true
			writes := aheadBeadWrites(t, f.bd)
			_, err := l.Land(context.Background(), f.work)
			if tc.wantInfra {
				var infra *InfraError
				if !errors.As(err, &infra) || errors.Is(err, ErrNoStoredVerdict) {
					t.Fatalf("Land error = %T %v; want an *InfraError that is not ErrNoStoredVerdict", err, err)
				}
			} else if !errors.Is(err, ErrNoStoredVerdict) {
				t.Fatalf("Land error = %v; want ErrNoStoredVerdict", err)
			}
			if !storedOnlyHasRecord(t, f) {
				t.Error("the record was deleted on an unknown answer; only a definite miss may delete it")
			}
			storedOnlyUntouched(t, f, calls, writes)
		})
	}
}

// TestVerdictReuseStoredOnlyMissDeleteFails: a store that will not remove a
// mismatched record fails the pass as infrastructure, not with the sentinel, which
// says "nothing to consume" while the record is still there (gt-6r4exl).
func TestVerdictReuseStoredOnlyMissDeleteFails(t *testing.T) {
	t.Parallel()
	f, l, store, key := reuseLandSetup(t)
	rec := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
	rec.Head = strings.Repeat("cd", 20)
	storedOnlyPut(t, store, rec)
	l.Ahead.removeFile = func(string) error { return errors.New("remove failed") }
	f.work.StoredOnly = true
	writes := aheadBeadWrites(t, f.bd)
	_, err := l.Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || errors.Is(err, ErrNoStoredVerdict) {
		t.Fatalf("Land error = %T %v; want an *InfraError that is not ErrNoStoredVerdict", err, err)
	}
	if !storedOnlyHasRecord(t, f) {
		t.Error("the record was removed despite the failure; want it left as it was")
	}
	storedOnlyUntouched(t, f, func() int { return len(f.review.calls) }, writes)
}

// TestVerdictReuseStoredOnlyConflictRejects: a stored conflict for this head
// that still conflicts rejects through today's conflict path — kind conflict,
// no om, no gate — and the record is deleted so it cannot outlive its
// rejection (gt-1yvte7). Keeping the record turns the last check red.
func TestVerdictReuseStoredOnlyConflictRejects(t *testing.T) {
	t.Parallel()
	f, l, store, key := reuseLandSetup(t)
	storedOnlyPut(t, store, storedOnlyConflict(key, f.base, l.now().Add(-time.Minute)))
	// Main moves onto the path the work adds, so the merge still conflicts.
	f.base = f.pushMain("b.txt", "main's b\n")
	f.work.StoredOnly = true
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectConflict, LabelRework)
	if n := len(f.review.calls); n != 0 {
		t.Errorf("om ran %d time(s) on a stored conflict; want 0", n)
	}
	if n := len(f.gate.dirs); n != 0 {
		t.Errorf("gate ran %d time(s) on a stored conflict rejection; want 0", n)
	}
	if storedOnlyHasRecord(t, f) {
		t.Error("the rejected conflict record was kept; it cannot outlive its rejection")
	}
}

// TestVerdictReuseStoredOnlyConflictGone: a stored conflict for this head that
// no longer conflicts leaves the pass with ErrNoStoredVerdict and deletes the
// record, with no om run and no gate (gt-1yvte7). Keeping the record, or
// reaching the review machinery, turns this red.
func TestVerdictReuseStoredOnlyConflictGone(t *testing.T) {
	t.Parallel()
	f, l, store, key := reuseLandSetup(t)
	storedOnlyPut(t, store, storedOnlyConflict(key, f.base, l.now().Add(-time.Minute)))
	// Only the clean merge may consume the record here: a pass that reached the
	// review could not answer from a conflict record, and the review assertions
	// below turn red if om runs.
	l.Reviewer = f.review
	// Main moves and the conflict is gone: the file main added over the work's
	// path is dropped again, so the merge adds the work's file cleanly.
	f.base = f.pushMain("b.txt", "main's b\n")
	f.base = f.git.CommitRemoving(t, f.origin, "main", "main: revert b", nil, "b.txt")
	f.work.StoredOnly = true
	writes := aheadBeadWrites(t, f.bd)
	_, err := l.Land(context.Background(), f.work)
	if !errors.Is(err, ErrNoStoredVerdict) {
		t.Fatalf("Land error = %v; want ErrNoStoredVerdict", err)
	}
	if storedOnlyHasRecord(t, f) {
		t.Error("the clean merge kept the conflict record; it is spent once the conflict is gone")
	}
	storedOnlyUntouched(t, f, func() int { return len(f.review.calls) }, writes)
}

// TestVerdictReuseStoredOnlyConflictStale: a stored conflict that cannot answer
// for this landing — another head, an expired record — is deleted and ends the
// pass with ErrNoStoredVerdict before it cuts a worktree or merges. A record
// that cannot be read is kept and fails the pass as infrastructure, not as
// "nothing to consume", and a delete that fails is infrastructure too
// (gt-1yvte7). Merging before the answer turns each row into a conflict
// rejection instead.
func TestVerdictReuseStoredOnlyConflictStale(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// editRecord changes the record before it is stored.
		editRecord func(*AheadRecord)
		// editLander changes the lander after the record is stored, for a seam
		// that would also steer the store's own write.
		editLander func(*Lander)
		// wantErr: the pass fails as infrastructure; kept: the record survives.
		wantErr bool
		kept    bool
	}{
		{name: "another head", editRecord: func(rec *AheadRecord) { rec.Head = strings.Repeat("cd", 20) }},
		{name: "an expired record", editRecord: func(rec *AheadRecord) {
			rec.ReviewedAt = rec.ReviewedAt.Add(-2 * time.Hour)
		}},
		{name: "a record that will not read", wantErr: true, kept: true, editLander: func(l *Lander) {
			l.Ahead.readFile = func(string) ([]byte, error) { return nil, errors.New("record unreadable") }
		}},
		{name: "a record that will not delete", wantErr: true, kept: true,
			editRecord: func(rec *AheadRecord) { rec.Head = strings.Repeat("cd", 20) },
			editLander: func(l *Lander) { l.Ahead.removeFile = func(string) error { return errors.New("remove failed") } }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, store, key := reuseLandSetup(t)
			rec := storedOnlyConflict(key, f.base, l.now().Add(-time.Minute))
			if tc.editRecord != nil {
				tc.editRecord(&rec)
			}
			storedOnlyPut(t, store, rec)
			if tc.editLander != nil {
				tc.editLander(l)
			}
			// Main moves onto the path the work adds: a pass that merges would
			// reject, so only a pass that ends before the merge returns the sentinel.
			f.base = f.pushMain("b.txt", "main's b\n")
			probe := reuseLandWorktreeProbeAt(t, f, l)
			f.work.StoredOnly = true
			writes := aheadBeadWrites(t, f.bd)
			_, err := l.Land(context.Background(), f.work)
			if tc.wantErr {
				var infra *InfraError
				if !errors.As(err, &infra) || errors.Is(err, ErrNoStoredVerdict) {
					t.Fatalf("Land error = %T %v; want an *InfraError that is not ErrNoStoredVerdict", err, err)
				}
			} else if !errors.Is(err, ErrNoStoredVerdict) {
				t.Fatalf("Land error = %v; want ErrNoStoredVerdict", err)
			}
			if probe.ran {
				t.Error("the pass cut a worktree and merged; a record that cannot answer for this landing must end the pass first")
			}
			if got := storedOnlyHasRecord(t, f); got != tc.kept {
				t.Errorf("the record is kept = %v; want %v", got, tc.kept)
			}
			storedOnlyUntouched(t, f, func() int { return len(f.review.calls) }, writes)
		})
	}
}

// TestVerdictReuseStoredOnlyConflictGoneDeleteFails: a clean merge whose
// conflict record cannot be deleted fails the pass as infrastructure, not with
// the sentinel that says "nothing to consume" while the record is still there
// (gt-1yvte7). Returning the sentinel turns this red.
func TestVerdictReuseStoredOnlyConflictGoneDeleteFails(t *testing.T) {
	t.Parallel()
	f, l, store, key := reuseLandSetup(t)
	storedOnlyPut(t, store, storedOnlyConflict(key, f.base, l.now().Add(-time.Minute)))
	f.base = f.pushMain("b.txt", "main's b\n")
	f.base = f.git.CommitRemoving(t, f.origin, "main", "main: revert b", nil, "b.txt")
	l.Ahead.removeFile = func(string) error { return errors.New("remove failed") }
	f.work.StoredOnly = true
	writes := aheadBeadWrites(t, f.bd)
	_, err := l.Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || errors.Is(err, ErrNoStoredVerdict) {
		t.Fatalf("Land error = %T %v; want an *InfraError that is not ErrNoStoredVerdict", err, err)
	}
	if !storedOnlyHasRecord(t, f) {
		t.Error("the record was removed despite the failure; want it left as it was")
	}
	storedOnlyUntouched(t, f, func() int { return len(f.review.calls) }, writes)
}

// TestVerdictReuseStoredOnlyConflictRejectDeleteFails: a stored conflict that
// still conflicts rejects even when its record cannot be deleted; the failure
// is logged and the rejection stands, so a store fault cannot keep the author's
// rework note from being written (gt-1yvte7). Turning the failure into an
// infrastructure error leaves the bead unrejected and turns this red.
func TestVerdictReuseStoredOnlyConflictRejectDeleteFails(t *testing.T) {
	t.Parallel()
	f, l, store, key := reuseLandSetup(t)
	storedOnlyPut(t, store, storedOnlyConflict(key, f.base, l.now().Add(-time.Minute)))
	f.base = f.pushMain("b.txt", "main's b\n")
	l.Ahead.removeFile = func(string) error { return errors.New("remove failed") }
	var out bytes.Buffer
	l.Out = &out
	f.work.StoredOnly = true
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectConflict, LabelRework)
	if !storedOnlyHasRecord(t, f) {
		t.Error("the record was removed despite the failure; want it left as it was")
	}
	if !strings.Contains(out.String(), "could not be deleted") {
		t.Errorf("landing log does not name the failed delete:\n%s", out.String())
	}
}

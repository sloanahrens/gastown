package landworker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// readySubmission is a ready bead whose READY TO LAND block names branch and
// head. It is not seeded: resolveWork reads the block from the bead itself.
func readySubmission(id, branch, head string) *beads.Issue {
	return &beads.Issue{ID: id, Labels: []string{land.LabelReadyToLand},
		Notes: land.FormatReadyNote(land.Work{Branch: branch, Head: head, Target: "main", Worker: "opal"})}
}

// TestReviewPoolWorkerReviewWorksBuilds pins that reviewWorks builds one work
// for each issue given, each with the branch and head of its own submission,
// so the pool reviews the head the landing will take (gt-uuvtbv).
func TestReviewPoolWorkerReviewWorksBuilds(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	first := readySubmission("gt-first", "polecat/opal/gt-first+x1", noteHead)
	second := readySubmission("gt-second", "polecat/opal/gt-second+x1", tipHead)

	works, err := h.w.reviewWorks([]*beads.Issue{first, second}, "")
	if err != nil {
		t.Fatalf("reviewWorks: %v", err)
	}
	if len(works) != 2 {
		t.Fatalf("reviewWorks returned %d works; want one for each of the 2 issues", len(works))
	}
	for i, want := range []struct{ bead, branch, head string }{
		{"gt-first", "polecat/opal/gt-first+x1", noteHead},
		{"gt-second", "polecat/opal/gt-second+x1", tipHead},
	} {
		got := works[i]
		if got.BeadID != want.bead || got.Branch != want.branch || got.Head != want.head {
			t.Fatalf("work %d = {bead %q branch %q head %q}; want {bead %q branch %q head %q}",
				i, got.BeadID, got.Branch, got.Head, want.bead, want.branch, want.head)
		}
		if got.Rig != h.w.Rig {
			t.Fatalf("work %d rig = %q; want the worker's rig %q", i, got.Rig, h.w.Rig)
		}
	}
}

// TestReviewPoolWorkerReviewWorksOrder pins that reviewWorks offers the works
// in the order the landing takes them, whatever order the issues arrive in: a
// P0 first, then the earliest submission, priority and then ID breaking a tie
// (gt-vh5psa).
func TestReviewPoolWorkerReviewWorksOrder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// Given newest first with the P0 in the middle, so the input order is
	// neither the landing order nor its reverse.
	issues := []*beads.Issue{
		readyIssue("gt-late", 2, "2026-09-30T13:00:00Z", "2026-09-30T13:00:00Z"),
		readyIssue("gt-hot", 0, "2026-09-30T11:00:00Z", "2026-09-30T11:00:00Z"),
		readyIssue("gt-tie-b", 2, "2026-09-30T10:00:00Z", "2026-09-30T10:00:00Z"),
		readyIssue("gt-tie-a", 2, "2026-09-30T10:00:00Z", "2026-09-30T10:00:00Z"),
		readyIssue("gt-old", 3, "2026-09-30T09:00:00Z", "2026-09-30T09:00:00Z"),
	}

	works, err := h.w.reviewWorks(issues, "")
	if err != nil {
		t.Fatalf("reviewWorks: %v", err)
	}
	if got, want := workOrder(works), "gt-hot,gt-old,gt-tie-a,gt-tie-b,gt-late"; got != want {
		t.Fatalf("reviewWorks order = %s; want %s", got, want)
	}
}

func workOrder(works []land.Work) string {
	ids := make([]string, 0, len(works))
	for _, w := range works {
		ids = append(ids, w.BeadID)
	}
	return strings.Join(ids, ",")
}

// TestReviewPoolWorkerReviewWorksReturnsUnreadableSubmission pins that an
// issue whose comments cannot be read fails the offer with that read error
// instead of vanishing from the list: what such a bead asks for is unknown,
// never absent (gt-uuvtbv).
func TestReviewPoolWorkerReviewWorksReturnsUnreadableSubmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	readable := readySubmission("gt-first", "polecat/opal/gt-first+x1", noteHead)
	// gt-unreadable carries no READY TO LAND block and is not in the store,
	// so the comments read fails.
	unreadable := &beads.Issue{ID: "gt-unreadable", Labels: []string{land.LabelReadyToLand}}

	works, err := h.w.reviewWorks([]*beads.Issue{readable, unreadable}, "")
	if err == nil {
		t.Fatalf("reviewWorks returned %d works and no error for an issue whose comments cannot be read", len(works))
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("reviewWorks error = %v; want the store's not-found error", err)
	}
	if !strings.Contains(err.Error(), "gt-unreadable") {
		t.Fatalf("reviewWorks error = %v; want it to name gt-unreadable", err)
	}
}

// TestReviewPoolWorkerReviewWorksRefusesUnresolvedSubmission pins that an
// issue whose comments read cleanly but name no submission (no READY TO LAND
// block, no submission comment) fails the offer naming that bead, rather than
// being dropped (gt-uuvtbv).
func TestReviewPoolWorkerReviewWorksRefusesUnresolvedSubmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	readable := readySubmission("gt-first", "polecat/opal/gt-first+x1", noteHead)
	bare := beads.Issue{ID: "gt-bare", Title: "work", Status: "hooked", Type: "task",
		Labels: []string{land.LabelReadyToLand}}
	h.bd.Seed(bare)

	works, err := h.w.reviewWorks([]*beads.Issue{readable, &bare}, "")
	if err == nil {
		t.Fatalf("reviewWorks returned %d works and no error for an issue naming no submission", len(works))
	}
	if !strings.Contains(err.Error(), "gt-bare") {
		t.Fatalf("reviewWorks error = %v; want it to name gt-bare", err)
	}
}

// TestLandingWorkerParksABeadWhoseCommentsCannotBeRead pins that process parks
// a ready bead whose submission cannot be read as an infrastructure failure
// instead of reporting it skipped: an unknown submission is never read as one
// that names nothing (gt-uuvtbv).
func TestLandingWorkerParksABeadWhoseCommentsCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	issue := &beads.Issue{ID: "gt-unreadable", Title: "work", Status: "hooked", Type: "task",
		Labels: []string{land.LabelReadyToLand}}
	h.bd.Seed(*issue)
	h.w.Beads = unreadableComments{h.bd}

	rep := Report{}
	h.w.process(context.Background(), issue, &rep)

	if want := (Report{Failed: 1}); rep != want {
		t.Fatalf("process report = %+v; want %+v: an unreadable submission is parked, not skipped", rep, want)
	}
}

// TestReviewPoolWorkerPoolDefaultsNil pins that the harness Worker leaves Pool
// nil and that a pass over two ready beads still lands both with today's
// report: the field is storage for later beads, and a nil pool changes nothing
// (gt-8vfrwi).
func TestReviewPoolWorkerPoolDefaultsNil(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if h.w.Pool != nil {
		t.Fatalf("harness Worker Pool = %#v, want nil", h.w.Pool)
	}
	h.seedReady(t, "gt-a")
	h.seedReady(t, "gt-b")
	rep := h.w.Pass(context.Background())
	if want := (Report{Landed: 2}); rep != want {
		t.Fatalf("pass report = %+v, want %+v", rep, want)
	}
}

// TestReviewPoolWorkerStoredDefaultsNil pins that the harness Worker leaves
// Stored nil and that a pass over two ready beads still lands both with
// today's report: the field is storage for later beads, and a nil hint source
// changes nothing (gt-5q8ayw).
func TestReviewPoolWorkerStoredDefaultsNil(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if h.w.Stored != nil {
		t.Fatalf("harness Worker Stored = %#v, want nil", h.w.Stored)
	}
	h.seedReady(t, "gt-a")
	h.seedReady(t, "gt-b")
	rep := h.w.Pass(context.Background())
	if want := (Report{Landed: 2}); rep != want {
		t.Fatalf("pass report = %+v, want %+v", rep, want)
	}
}

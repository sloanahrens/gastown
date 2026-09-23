package refinery

import (
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// TestRejectionNoteRoundTrip_FindingsSurviveBuildPriorFindings is gt-s4f6's
// acceptance criterion: the MERGE REJECTION note the refinery writes onto a
// source bead, read back by the same builder the rework prompt uses, yields
// the findings that went in.
//
// The two halves live in different packages (refinery writes, editorial
// parses) and were written separately, so each half's own test passed while
// the pair did not agree — a rejected MR carried no finding block the next
// attempt could see, and the same defect was re-merged. This test drives the
// real recovery path end to end: format the note, take the exact bytes the
// bead receives, and run them through BuildPriorFindings.
func TestRejectionNoteRoundTrip_FindingsSurviveBuildPriorFindings(t *testing.T) {
	t.Parallel()
	findings := []RejectionFinding{
		{ID: "cb332644e4cf", Severity: "major", Path: "internal/hooks/config.go", Line: 432, Title: "boot hook override has no self-filtering path"},
		{ID: "cc825768ed16", Severity: "minor", Path: "internal/hooks/config_test.go", Line: 898, Title: "test rewritten to agree with the regression"},
		{ID: "0f1e2d3c4b5a", Severity: "info", Path: "internal/x.go", Line: 1, Title: "empty-ish title with a trailing colon: like this"},
	}

	notes := rejectionNotesWrittenToBead(t, findings)

	got := editorial.BuildPriorFindings(beadsForNotes(t, "gt-src1", notes), "gt-src1", 7)

	if len(got) != len(findings) {
		t.Fatalf("round trip lost findings: wrote %d, parsed %d\nnotes:\n%s\ngot: %+v",
			len(findings), len(got), notes, got)
	}
	for i, want := range findings {
		if got[i].ID != want.ID || got[i].Severity != want.Severity ||
			got[i].Path != want.Path || got[i].Line != want.Line || got[i].Title != want.Title {
			t.Errorf("finding[%d] round-tripped as %+v, want %+v", i, got[i], want)
		}
		if got[i].Attempt != 7 {
			t.Errorf("finding[%d] Attempt = %d, want 7", i, got[i].Attempt)
		}
	}
}

// TestRejectionNoteRoundTrip_EmptyTitle covers the finding whose title the
// note writer leaves empty: BuildPriorFindings must still return the finding
// rather than dropping the whole line (gt-j6ez).
func TestRejectionNoteRoundTrip_EmptyTitle(t *testing.T) {
	t.Parallel()
	findings := []RejectionFinding{
		{ID: "abc123def456", Severity: "major", Path: "internal/foo.go", Line: 42},
	}

	notes := rejectionNotesWrittenToBead(t, findings)

	got := editorial.BuildPriorFindings(beadsForNotes(t, "gt-src1", notes), "gt-src1", 1)

	if len(got) != 1 {
		t.Fatalf("parsed %d findings, want 1:\n%s", len(got), notes)
	}
	if got[0].ID != "abc123def456" || got[0].Path != "internal/foo.go" || got[0].Line != 42 || got[0].Title != "" {
		t.Errorf("finding = %+v, want id abc123def456 path internal/foo.go line 42 with an empty title", got[0])
	}
}

// TestRejectionNoteRoundTrip_AppendsToPolecatNotes drives the case the notes
// contract actually has to survive: the polecat's own implementation notes are
// already on the bead, and the rejection is appended to them (gt-nxvg). Both
// blocks must come out the other side, with the finding parsed.
func TestRejectionNoteRoundTrip_AppendsToPolecatNotes(t *testing.T) {
	t.Parallel()
	const priorNotes = "Findings so far: built the parser, no tests yet"
	findings := []RejectionFinding{
		{ID: "abc123def456", Severity: "major", Path: "internal/foo.go", Line: 42, Title: "unchecked error"},
	}

	notes := rejectionNotesWrittenToBeadFrom(t, priorNotes, findings)

	if !strings.Contains(notes, priorNotes) {
		t.Fatalf("rejection note clobbered the polecat's own notes:\n%s", notes)
	}
	got := editorial.BuildPriorFindings(beadsForNotes(t, "gt-src1", notes), "gt-src1", 2)
	if len(got) != 1 || got[0].ID != "abc123def456" {
		t.Fatalf("parsed %+v, want the one appended finding:\n%s", got, notes)
	}
}

// TestRecordRejectionFindings_RoundTrip is the round trip for the path
// `gt mq reject --findings-json` drives: a rejection recorded through
// recordRejectionFindings lands a note that BuildPriorFindings reads back as
// the same findings.
//
// This is the path an editorial rejection takes when the caller redispatches
// itself. It is not the dead-worker one — that path only writes a note when
// the worker has lost the bead, so on its own it leaves a rejection of a live
// worker's branch with no findings anywhere (gt-s4f6).
func TestRecordRejectionFindings_RoundTrip(t *testing.T) {
	t.Parallel()
	findings := []RejectionFinding{
		{ID: "cb332644e4cf", Severity: "major", Path: "internal/hooks/config.go", Line: 432, Title: "boot hook override has no self-filtering path"},
		{ID: "cc825768ed16", Severity: "minor", Path: "internal/hooks/config_test.go", Line: 898, Title: "test rewritten to agree with the regression"},
	}
	mr := &MergeRequest{
		ID:           "gt-mr-1",
		Branch:       "polecat/flint/gt-3mp1+mu7l2qvj",
		Worker:       "polecats/flint",
		IssueID:      "gt-src1",
		TargetBranch: "main",
		RetryCount:   1,
	}
	bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "open", Assignee: "flint"}}

	recordRejectionFindings(bd, rejectionNoteRequest(mr, "gastown", "EDITORIAL REJECTION (attempt 2): om gate request_changes", findings), func(string, ...interface{}) {})

	if len(bd.runCalls) != 1 {
		t.Fatalf("expected 1 notes write, got %d: %v", len(bd.runCalls), bd.runCalls)
	}
	notes := bd.runCalls[0][len(bd.runCalls[0])-1]
	for _, want := range []string{
		MergeRejectionNoteMarker,
		"Branch: polecat/flint/gt-3mp1+mu7l2qvj",
		"Target: main",
		"MR: gt-mr-1",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("note missing %q; the mol-polecat-work resume path greps for it:\n%s", want, notes)
		}
	}

	got := editorial.BuildPriorFindings(beadsForNotes(t, "gt-src1", notes), "gt-src1", 2)
	if len(got) != len(findings) {
		t.Fatalf("round trip lost findings: wrote %d, parsed %d\nnotes:\n%s\ngot: %+v", len(findings), len(got), notes, got)
	}
	for i, want := range findings {
		if got[i].ID != want.ID || got[i].Path != want.Path || got[i].Line != want.Line || got[i].Title != want.Title {
			t.Errorf("finding[%d] round-tripped as %+v, want %+v", i, got[i], want)
		}
	}
}

// TestRecordRejectionFindings_SkipsRepeatWrite covers the refinery re-running
// the same rejection: an identical note is already on the bead, so the second
// record is a no-op rather than a second copy of the block.
func TestRecordRejectionFindings_SkipsRepeatWrite(t *testing.T) {
	t.Parallel()
	mr := &MergeRequest{ID: "gt-mr-1", Branch: "b", IssueID: "gt-src1", TargetBranch: "main"}
	req := rejectionNoteRequest(mr, "gastown", "rejected", nil)
	bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "open", Notes: formatMergeRejectionNote(req)}}

	recordRejectionFindings(bd, req, func(string, ...interface{}) {})

	if len(bd.runCalls) != 0 {
		t.Fatalf("expected no second write, got %d: %v", len(bd.runCalls), bd.runCalls)
	}
}

// rejectionNotesWrittenToBead runs the real rejection path against an empty
// source bead and returns the notes string the bead would end up with.
func rejectionNotesWrittenToBead(t *testing.T, findings []RejectionFinding) string {
	t.Helper()
	return rejectionNotesWrittenToBeadFrom(t, "", findings)
}

// rejectionNotesWrittenToBeadFrom is rejectionNotesWrittenToBead with the
// source bead's pre-existing notes as the starting point.
func rejectionNotesWrittenToBeadFrom(t *testing.T, existingNotes string, findings []RejectionFinding) string {
	t.Helper()
	bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "closed", Notes: existingNotes}}
	sendMail := func(*mail.Message) error { return nil }

	req := deadWorkerReq()
	req.SourceIssue = "gt-src1"
	req.Findings = findings
	if !recoverRejectedMRDeadWorker(bd, deadSession, sendMail, nil, req) {
		t.Fatal("expected dead-worker recovery to run")
	}
	if len(bd.runCalls) != 1 {
		t.Fatalf("expected exactly 1 notes write, got %d: %v", len(bd.runCalls), bd.runCalls)
	}
	args := bd.runCalls[0]
	return args[len(args)-1]
}

// beadsForNotes wraps a store holding one issue, so BuildPriorFindings reads
// the notes exactly as it would from a real bead.
func beadsForNotes(t *testing.T, id, notes string) *beads.Beads {
	t.Helper()
	now := time.Now()
	return testStoreBeads(t, newBatchReviewStore(&beadsdk.Issue{
		ID:        id,
		Title:     id,
		Notes:     notes,
		Status:    beadsdk.StatusClosed,
		IssueType: beadsdk.IssueType("task"),
		CreatedAt: now,
		UpdatedAt: now,
	}))
}

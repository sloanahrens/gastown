package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// TestRejectRecord_ClassComesFromTheFlags pins what `gt mq reject` records
// about why it rejected the branch (gt-1jig): an om verdict's --findings-json
// classifies its own rejection as editorial, --failure-type states whatever
// the caller knows (over the verdict, for the branch whose real defect the
// verdict only described in prose), and neither flag records no class at all
// instead of a defaulted one.
func TestRejectRecord_ClassComesFromTheFlags(t *testing.T) {
	t.Parallel()
	verdict := reviewJSONFile(t, editorial.ReviewResult{
		Exit: 1,
		Note: &editorial.Note{
			Verdict: "request_changes",
			Score:   0.42,
			Findings: []editorial.Finding{
				{ID: "cb332644e4cf", Severity: "major", Path: "internal/hooks/config.go", Line: 432, Title: "no self-filtering path"},
			},
		},
	})

	cases := []struct {
		name       string
		findings   string
		failure    string
		wantClass  string
		wantCounts bool
	}{
		{name: "findings are an om verdict, so editorial", findings: verdict, wantClass: refinery.FailureTypeEditorial, wantCounts: true},
		{name: "a stated class overrides the verdict's", findings: verdict, failure: "build", wantClass: refinery.FailureTypeBuild, wantCounts: true},
		{name: "a stated class with no verdict behind it", failure: "tests", wantClass: refinery.FailureTypeTests},
		{name: "neither flag classifies nothing", wantClass: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := rejectRecord(tc.findings, tc.failure, 0)
			if err != nil {
				t.Fatalf("rejectRecord() error: %v", err)
			}
			if rec.FailureType != tc.wantClass {
				t.Errorf("FailureType = %q, want %q", rec.FailureType, tc.wantClass)
			}
			// The verdict's findings and receipt ride along whatever the class
			// is: they are what the next attempt reads back.
			if got := rec.Receipt != nil; got != tc.wantCounts {
				t.Errorf("carries a receipt = %v, want %v", got, tc.wantCounts)
			}
		})
	}
}

// TestRejectRecord_InfraFailureClassifiesNothing covers the review result that
// produced no verdict: an infra failure says nothing about the branch, so it
// must not be recorded as an editorial rejection either.
func TestRejectRecord_InfraFailureClassifiesNothing(t *testing.T) {
	t.Parallel()
	path := reviewJSONFile(t, editorial.ReviewResult{Exit: 2, Class: editorial.Tooling, Stderr: "backend timeout"})

	rec, err := rejectRecord(path, "", 0)
	if err != nil {
		t.Fatalf("rejectRecord() error: %v", err)
	}
	if rec.FailureType != "" {
		t.Errorf("FailureType = %q, want none for a result that produced no verdict", rec.FailureType)
	}
}

// TestRejectRecord_RefusesUnknownClass is the refusal half of gt-1jig: the
// class is durable history a later reader triages by, so a typo'd one fails
// while the MR is still open and the command can be corrected, rather than
// being recorded under a name no reader knows.
func TestRejectRecord_RefusesUnknownClass(t *testing.T) {
	t.Parallel()
	_, err := rejectRecord("", "regression", 0)
	if err == nil {
		t.Fatal("expected an error for a class outside the vocabulary")
	}
	if !strings.Contains(err.Error(), "--failure-type") {
		t.Errorf("error %q does not name the flag that produced it", err)
	}
}

// TestRejectRecord_RefusesUnreadableFindings keeps the pre-existing refusal
// beside the new one: a malformed verdict fails before anything is rejected,
// not after the MR is closed (gt-s4f6).
func TestRejectRecord_RefusesUnreadableFindings(t *testing.T) {
	t.Parallel()
	if _, err := rejectRecord(t.TempDir()+"/absent.json", "", 0); err == nil {
		t.Fatal("expected an error for an unreadable findings file")
	}
}

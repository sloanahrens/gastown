package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// reviewJSONFile writes a review result the way `gt mq review --json` emits it
// — encoding the real type, so this test fails if that output shape moves out
// from under --findings-json.
func reviewJSONFile(t *testing.T, result editorial.ReviewResult) string {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshalling review result: %v", err)
	}
	path := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing review json: %v", err)
	}
	return path
}

// TestReadRejectFindings_ParsesReviewResult is the reject end of gt-s4f6: the
// findings on an om verdict reach a rejection's durable record, in the shape
// the parser reads back.
func TestReadRejectFindings_ParsesReviewResult(t *testing.T) {
	t.Parallel()
	path := reviewJSONFile(t, editorial.ReviewResult{
		Exit: 1,
		Note: &editorial.Note{
			Verdict:       "request_changes",
			Score:         0.42,
			FindingsCount: 2,
			Findings: []editorial.Finding{
				{ID: "cb332644e4cf", Severity: "major", Path: "internal/hooks/config.go", Line: 432, Title: "boot hook override has no self-filtering path"},
				{ID: "cc825768ed16", Severity: "minor", Path: "internal/hooks/config_test.go", Line: 898, Title: "test rewritten to agree with the regression"},
			},
		},
	})

	got, err := readRejectFindings(path)
	if err != nil {
		t.Fatalf("readRejectFindings() error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
	if got[0].ID != "cb332644e4cf" || got[0].Severity != "major" ||
		got[0].Path != "internal/hooks/config.go" || got[0].Line != 432 ||
		got[0].Title != "boot hook override has no self-filtering path" {
		t.Errorf("finding[0] = %+v, unexpected", got[0])
	}
	if got[1].ID != "cc825768ed16" {
		t.Errorf("finding[1] = %+v, unexpected", got[1])
	}
}

// TestReadRejectFindings_InfraFailureHasNone covers the exit-2 result: no note
// means no verdict, so there are no findings to record — and that is not an
// error, because the caller decides what an infra failure means.
func TestReadRejectFindings_InfraFailureHasNone(t *testing.T) {
	t.Parallel()
	path := reviewJSONFile(t, editorial.ReviewResult{Exit: 2, Class: editorial.Tooling, Stderr: "backend timeout"})

	got, err := readRejectFindings(path)
	if err != nil {
		t.Fatalf("readRejectFindings() error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want no findings from a result that produced no note", got)
	}
}

// TestReadRejectFindings_RejectsOtherJSON names the expected input in the
// error: a caller who passes something else (a raw om verdict, an MR bead)
// needs to be told what this flag reads.
func TestReadRejectFindings_RejectsOtherJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(path, []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	_, err := readRejectFindings(path)
	if err == nil {
		t.Fatal("expected an error for a file that is not a review result")
	}
	if !strings.Contains(err.Error(), "gt mq review --json") {
		t.Errorf("error %q does not name the input it expects", err)
	}
}

// TestReadRejectFindings_MissingFile reports the read failure rather than
// silently rejecting with no findings.
func TestReadRejectFindings_MissingFile(t *testing.T) {
	t.Parallel()
	if _, err := readRejectFindings(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected an error for a missing findings file")
	}
}

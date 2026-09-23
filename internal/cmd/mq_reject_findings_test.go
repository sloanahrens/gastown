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
	if len(got.Findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got.Findings), got.Findings)
	}
	if got.Findings[0].ID != "cb332644e4cf" || got.Findings[0].Severity != "major" ||
		got.Findings[0].Path != "internal/hooks/config.go" || got.Findings[0].Line != 432 ||
		got.Findings[0].Title != "boot hook override has no self-filtering path" {
		t.Errorf("finding[0] = %+v, unexpected", got.Findings[0])
	}
	if got.Findings[1].ID != "cc825768ed16" {
		t.Errorf("finding[1] = %+v, unexpected", got.Findings[1])
	}
	// The score and unresolved ids ride along so `gt deacon redispatch` can
	// run its convergence rule instead of falling back to attempt counting.
	if got.Receipt == nil {
		t.Fatal("expected a Receipt carrying the verdict's score and unresolved ids")
	}
	if got.Receipt.Score != 0.42 {
		t.Errorf("Receipt.Score = %v, want 0.42", got.Receipt.Score)
	}
}

// TestReadRejectFindings_CarriesUnresolvedIDs pins the half of the receipt the
// deacon's convergence rule keys on: the ids om reports as still open.
func TestReadRejectFindings_CarriesUnresolvedIDs(t *testing.T) {
	t.Parallel()
	note := &editorial.Note{
		Verdict: "request_changes",
		Score:   0.31,
		Findings: []editorial.Finding{
			{ID: "cb332644e4cf", Severity: "major", Path: "internal/hooks/config.go", Line: 432, Title: "no self-filtering path"},
		},
	}
	note.PriorFindings.Unresolved = []string{"cb332644e4cf", "deadbeef1234"}
	path := reviewJSONFile(t, editorial.ReviewResult{Exit: 1, Note: note})

	got, err := readRejectFindings(path)
	if err != nil {
		t.Fatalf("readRejectFindings() error: %v", err)
	}
	if got.Receipt == nil {
		t.Fatal("expected a Receipt")
	}
	want := []string{"cb332644e4cf", "deadbeef1234"}
	if len(got.Receipt.Unresolved) != len(want) {
		t.Fatalf("Receipt.Unresolved = %v, want %v", got.Receipt.Unresolved, want)
	}
	for i, id := range want {
		if got.Receipt.Unresolved[i] != id {
			t.Errorf("Receipt.Unresolved[%d] = %q, want %q", i, got.Receipt.Unresolved[i], id)
		}
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
	if len(got.Findings) != 0 {
		t.Errorf("got %+v, want no findings from a result that produced no note", got)
	}
	if got.Receipt != nil {
		t.Errorf("Receipt = %+v, want nil — an infra failure produced no verdict to carry", got.Receipt)
	}
}

// TestRejectReason_RefusesTwoStdinReaders covers the one flag conflict that is
// not about the reason alone: --stdin and --findings-json - both read stdin, so
// the second one would see whatever the first left (gt-s4f6).
func TestRejectReason_RefusesTwoStdinReaders(t *testing.T) {
	t.Parallel()
	_, err := rejectReason(true, "", "-", strings.NewReader("reason from stdin"))
	if err == nil {
		t.Fatal("expected an error for --stdin with --findings-json -")
	}
	if !strings.Contains(err.Error(), "findings-json") {
		t.Errorf("error %q does not name the conflicting flag", err)
	}
}

// TestRejectReason_ReadsReasonFromStdin covers the happy path through the
// extraction: the reason arrives on stdin, trimmed of the heredoc's trailing
// newline.
func TestRejectReason_ReadsReasonFromStdin(t *testing.T) {
	t.Parallel()
	got, err := rejectReason(true, "", "/tmp/review.json", strings.NewReader("EDITORIAL REJECTION (attempt 2): request_changes\n"))
	if err != nil {
		t.Fatalf("rejectReason() error: %v", err)
	}
	if got != "EDITORIAL REJECTION (attempt 2): request_changes" {
		t.Errorf("reason = %q, want the stdin text with its trailing newline trimmed", got)
	}
}

// TestRejectReason_RejectsMissingReason guards the refusal that would otherwise
// reach the refinery with an empty reason.
func TestRejectReason_RejectsMissingReason(t *testing.T) {
	t.Parallel()
	if _, err := rejectReason(false, "", "/tmp/review.json", strings.NewReader("")); err == nil {
		t.Fatal("expected an error when no reason is supplied")
	}
}

// TestRejectReason_RejectsStdinWithReason keeps the two reason sources from
// being silently merged.
func TestRejectReason_RejectsStdinWithReason(t *testing.T) {
	t.Parallel()
	if _, err := rejectReason(true, "-r reason", "", strings.NewReader("stdin reason")); err == nil {
		t.Fatal("expected an error for --stdin with --reason")
	}
}

// TestReadRejectFindings_MalformedFailsBeforeAnyRejection pins the ordering the
// parser depends on: runMQReject reads --findings-json before it constructs a
// manager, so a bad file cannot leave a rejection whose record failed. Here
// that is observable as the error naming the file, not the MR.
func TestReadRejectFindings_MalformedFailsBeforeAnyRejection(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"Exit":1,"Note":{"score":"not a number"}}`), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	_, err := readRejectFindings(path)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if !strings.Contains(err.Error(), "bad.json") {
		t.Errorf("error %q does not name the file it could not read", err)
	}
}

// TestReadRejectFindings_StdinRecordsWhereTheFlagSays covers `--findings-json -`:
// the verdict arrives on stdin, and the record it builds is the same one the
// file form builds.
func TestReadRejectFindings_StdinRecordsWhereTheFlagSays(t *testing.T) {
	raw, err := json.Marshal(editorial.ReviewResult{
		Exit: 1,
		Note: &editorial.Note{
			Verdict: "request_changes",
			Score:   0.2,
			Findings: []editorial.Finding{
				{ID: "cb332644e4cf", Severity: "major", Path: "internal/hooks/config.go", Line: 432, Title: "no self-filtering path"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshalling review result: %v", err)
	}
	restoreStdin := swapStdin(t, string(raw))
	defer restoreStdin()

	got, err := readRejectFindings("-")
	if err != nil {
		t.Fatalf("readRejectFindings(-) error: %v", err)
	}
	if len(got.Findings) != 1 || got.Findings[0].ID != "cb332644e4cf" {
		t.Errorf("Findings = %+v, want the verdict's one finding", got.Findings)
	}
}

// swapStdin points os.Stdin at a pipe holding data, and returns a restore func.
func swapStdin(t *testing.T, data string) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := w.WriteString(data); err != nil {
		t.Fatalf("writing stdin: %v", err)
	}
	_ = w.Close()
	orig := os.Stdin
	os.Stdin = r
	return func() {
		os.Stdin = orig
		_ = r.Close()
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

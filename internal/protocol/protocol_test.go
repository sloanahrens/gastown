package protocol

import (
	"testing"
	"time"
)

func TestParseMessageType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subject  string
		expected MessageType
	}{
		{"MERGE_READY nux", TypeMergeReady},
		{"MERGED Toast", TypeMerged},
		{"MERGE_FAILED ace", TypeMergeFailed},
		{"REWORK_REQUEST valkyrie", TypeReworkRequest},
		{"MERGE_READY", TypeMergeReady}, // no polecat name
		{"Unknown subject", ""},
		{"", ""},
		{"  MERGE_READY nux  ", TypeMergeReady}, // with whitespace
		{"MERGEDFOO", ""},                       // prefix without space delimiter
		{"MERGE_READYBAR", ""},                  // prefix without space delimiter
		{"MERGE_FAILEDX", ""},                   // prefix without space delimiter
		{"REWORK_REQUESTZ", ""},                 // prefix without space delimiter
	}

	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			result := ParseMessageType(tt.subject)
			if result != tt.expected {
				t.Errorf("ParseMessageType(%q) = %q, want %q", tt.subject, result, tt.expected)
			}
		})
	}
}

func TestExtractPolecat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subject  string
		expected string
	}{
		{"MERGE_READY nux", "nux"},
		{"MERGED Toast", "Toast"},
		{"MERGE_FAILED ace", "ace"},
		{"REWORK_REQUEST valkyrie", "valkyrie"},
		{"MERGE_READY", ""},
		{"", ""},
		{"  MERGE_READY nux  ", "nux"},
	}

	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			result := ExtractPolecat(tt.subject)
			if result != tt.expected {
				t.Errorf("ExtractPolecat(%q) = %q, want %q", tt.subject, result, tt.expected)
			}
		})
	}
}

func TestIsProtocolMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subject  string
		expected bool
	}{
		{"MERGE_READY nux", true},
		{"MERGED Toast", true},
		{"MERGE_FAILED ace", true},
		{"REWORK_REQUEST valkyrie", true},
		{"CONVOY_NEEDS_FEEDING hq-cv123", true},
		{"Unknown subject", false},
		{"", false},
		{"Hello world", false},
	}

	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			result := IsProtocolMessage(tt.subject)
			if result != tt.expected {
				t.Errorf("IsProtocolMessage(%q) = %v, want %v", tt.subject, result, tt.expected)
			}
		})
	}
}

func TestParseMergeReadyPayload(t *testing.T) {
	t.Parallel()
	body := `Branch: polecat/nux/gt-abc
Issue: gt-abc
Polecat: nux
Rig: gastown
Verified: clean git state`

	payload, err := ParseMergeReadyPayload(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if payload.Branch != "polecat/nux/gt-abc" {
		t.Errorf("Branch = %q, want %q", payload.Branch, "polecat/nux/gt-abc")
	}
	if payload.Issue != "gt-abc" {
		t.Errorf("Issue = %q, want %q", payload.Issue, "gt-abc")
	}
	if payload.Polecat != "nux" {
		t.Errorf("Polecat = %q, want %q", payload.Polecat, "nux")
	}
	if payload.Rig != "gastown" {
		t.Errorf("Rig = %q, want %q", payload.Rig, "gastown")
	}
}

func TestParseMessageType_ConvoyNeedsFeeding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subject  string
		expected MessageType
	}{
		{"CONVOY_NEEDS_FEEDING hq-cv123", TypeConvoyNeedsFeeding},
		{"CONVOY_NEEDS_FEEDING", TypeConvoyNeedsFeeding},
		{"CONVOY_NEEDS_FEEDINGX", ""},
	}

	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			result := ParseMessageType(tt.subject)
			if result != tt.expected {
				t.Errorf("ParseMessageType(%q) = %q, want %q", tt.subject, result, tt.expected)
			}
		})
	}
}

func TestParseConvoyNeedsFeedingPayload(t *testing.T) {
	t.Parallel()
	ts := time.Now().Format(time.RFC3339)
	body := "ConvoyID: hq-cv123\nSourceIssue: gt-abc\nRig: gastown\nMerged-At: " + ts

	payload, err := ParseConvoyNeedsFeedingPayload(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if payload.ConvoyID != "hq-cv123" {
		t.Errorf("ConvoyID = %q, want %q", payload.ConvoyID, "hq-cv123")
	}
	if payload.SourceIssue != "gt-abc" {
		t.Errorf("SourceIssue = %q, want %q", payload.SourceIssue, "gt-abc")
	}
	if payload.Rig != "gastown" {
		t.Errorf("Rig = %q, want %q", payload.Rig, "gastown")
	}
}

func TestParseConvoyNeedsFeedingPayload_InvalidInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"missing convoy id", "Rig: gastown\nSourceIssue: gt-abc"},
		{"missing rig", "ConvoyID: hq-cv123\nSourceIssue: gt-abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := ParseConvoyNeedsFeedingPayload(tt.body)
			if err == nil {
				t.Errorf("expected error for body %q, got payload: %+v", tt.body, payload)
			}
			if payload != nil {
				t.Errorf("expected nil payload on error, got: %+v", payload)
			}
		})
	}
}

func TestParseMergeReadyPayload_InvalidInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"missing all fields", "Hello: world"},
		{"missing branch", "Polecat: nux\nRig: gastown"},
		{"missing polecat", "Branch: polecat/nux\nRig: gastown"},
		{"missing rig", "Branch: polecat/nux\nPolecat: nux"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := ParseMergeReadyPayload(tt.body)
			if err == nil {
				t.Errorf("expected error for body %q, got payload: %+v", tt.body, payload)
			}
			if payload != nil {
				t.Errorf("expected nil payload on error, got: %+v", payload)
			}
		})
	}
}

func TestParseMergedPayload(t *testing.T) {
	t.Parallel()
	ts := time.Now().Format(time.RFC3339)
	body := `Branch: polecat/nux/gt-abc
Issue: gt-abc
Polecat: nux
Rig: gastown
Target: main
Merged-At: ` + ts + `
Merge-Commit: abc123`

	payload, err := ParseMergedPayload(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if payload.Branch != "polecat/nux/gt-abc" {
		t.Errorf("Branch = %q, want %q", payload.Branch, "polecat/nux/gt-abc")
	}
	if payload.MergeCommit != "abc123" {
		t.Errorf("MergeCommit = %q, want %q", payload.MergeCommit, "abc123")
	}
	if payload.TargetBranch != "main" {
		t.Errorf("TargetBranch = %q, want %q", payload.TargetBranch, "main")
	}
}

func TestParseMergedPayload_InvalidInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"missing polecat", "Branch: polecat/nux\nRig: gastown"},
		{"missing rig", "Branch: polecat/nux\nPolecat: nux"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := ParseMergedPayload(tt.body)
			if err == nil {
				t.Errorf("expected error for body %q, got payload: %+v", tt.body, payload)
			}
			if payload != nil {
				t.Errorf("expected nil payload on error, got: %+v", payload)
			}
		})
	}
}

func TestParseMergeFailedPayload(t *testing.T) {
	t.Parallel()
	body := `Branch: polecat/nux/gt-abc
Issue: gt-abc
Polecat: nux
Rig: gastown
Target: main
Failure-Type: tests
Error: Test failed`

	payload, err := ParseMergeFailedPayload(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if payload.Branch != "polecat/nux/gt-abc" {
		t.Errorf("Branch = %q, want %q", payload.Branch, "polecat/nux/gt-abc")
	}
	if payload.FailureType != "tests" {
		t.Errorf("FailureType = %q, want %q", payload.FailureType, "tests")
	}
	if payload.Error != "Test failed" {
		t.Errorf("Error = %q, want %q", payload.Error, "Test failed")
	}
}

func TestParseMergeFailedPayload_InvalidInput(t *testing.T) {
	t.Parallel()
	payload, err := ParseMergeFailedPayload("")
	if err == nil {
		t.Errorf("expected error for empty body, got payload: %+v", payload)
	}
	if payload != nil {
		t.Errorf("expected nil payload on error, got: %+v", payload)
	}
}

func TestParseReworkRequestPayload(t *testing.T) {
	t.Parallel()
	body := `Branch: polecat/nux/gt-abc
Issue: gt-abc
Polecat: nux
Rig: gastown
Target: main
Conflict-Files: file1.go, file2.go`

	payload, err := ParseReworkRequestPayload(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if payload.Branch != "polecat/nux/gt-abc" {
		t.Errorf("Branch = %q, want %q", payload.Branch, "polecat/nux/gt-abc")
	}
	if payload.TargetBranch != "main" {
		t.Errorf("TargetBranch = %q, want %q", payload.TargetBranch, "main")
	}
	if len(payload.ConflictFiles) != 2 {
		t.Errorf("ConflictFiles length = %d, want 2", len(payload.ConflictFiles))
	}
}

func TestParseReworkRequestPayload_InvalidInput(t *testing.T) {
	t.Parallel()
	payload, err := ParseReworkRequestPayload("")
	if err == nil {
		t.Errorf("expected error for empty body, got payload: %+v", payload)
	}
	if payload != nil {
		t.Errorf("expected nil payload on error, got: %+v", payload)
	}
}

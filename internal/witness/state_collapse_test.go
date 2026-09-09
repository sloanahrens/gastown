package witness

import (
	"strings"
	"testing"
)

func TestDetectStateCollapse_NilBd(t *testing.T) {
	t.Parallel()
	result := DetectStateCollapse(nil, "/nonexistent", "testrig", nil)
	if result.Checked != 0 || len(result.Findings) != 0 || len(result.Errors) != 0 {
		t.Errorf("DetectStateCollapse(nil) = %+v, want empty result", result)
	}
}

func TestDetectStateCollapse_ListError(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			return "", errFakeListFailure
		},
		func(args []string) error { return nil },
	)

	result := DetectStateCollapse(bd, "/work", "testrig", nil)
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1", len(result.Errors))
	}
	if result.Checked != 0 || len(result.Findings) != 0 {
		t.Errorf("expected no findings on list error, got %+v", result)
	}
}

func TestDetectStateCollapse_NoOpenMRs(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 0 && args[0] == "list" {
				return "[]", nil
			}
			return "{}", nil
		},
		func(args []string) error { return nil },
	)

	result := DetectStateCollapse(bd, "/work", "testrig", nil)
	if result.Checked != 0 || len(result.Findings) != 0 || len(result.Errors) != 0 {
		t.Errorf("expected empty result with no open MRs, got %+v", result)
	}
	logStr := strings.Join(mock.calls, "\n")
	if !strings.Contains(logStr, "--label=gt:merge-request") || !strings.Contains(logStr, "--status=open") {
		t.Errorf("bd list was not called with expected filters; log:\n%s", logStr)
	}
}

// TestDetectStateCollapse_ClosedSourceIssue is the core regression case: an
// open MR whose source issue is closed is the gt-zzd instance-4/6 signature
// (bead closed, MR open, fix not verified in force) and must be flagged.
func TestDetectStateCollapse_ClosedSourceIssue(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) == 0 {
				return "{}", nil
			}
			switch args[0] {
			case "list":
				return `[
  {"id":"gt-mr-collapsed","status":"open","description":"branch: polecat/topaz/gt-wdr+abc\ntarget: main\nsource_issue: gt-wdr\n"},
  {"id":"gt-mr-healthy","status":"open","description":"branch: polecat/jade/gt-ok+def\ntarget: main\nsource_issue: gt-ok\n"}
]`, nil
			case "show":
				if len(args) > 1 && args[1] == "gt-wdr" {
					return `[{"status":"closed"}]`, nil
				}
				if len(args) > 1 && args[1] == "gt-ok" {
					return `[{"status":"open"}]`, nil
				}
				return "[]", nil
			}
			return "{}", nil
		},
		func(args []string) error { return nil },
	)

	result := DetectStateCollapse(bd, "/work", "gastown", nil)

	if result.Checked != 2 {
		t.Errorf("Checked = %d, want 2", result.Checked)
	}
	if len(result.Errors) != 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1: %+v", len(result.Findings), result.Findings)
	}

	f := result.Findings[0]
	if f.IssueID != "gt-wdr" {
		t.Errorf("IssueID = %q, want gt-wdr", f.IssueID)
	}
	if f.MRID != "gt-mr-collapsed" {
		t.Errorf("MRID = %q, want gt-mr-collapsed", f.MRID)
	}
	if f.MRStatus != "open" {
		t.Errorf("MRStatus = %q, want open", f.MRStatus)
	}
	if f.Branch != "polecat/topaz/gt-wdr+abc" {
		t.Errorf("Branch = %q, want polecat/topaz/gt-wdr+abc", f.Branch)
	}
	if f.Target != "main" {
		t.Errorf("Target = %q, want main", f.Target)
	}

	// A finding must be persisted as a bead comment on the source issue.
	logStr := strings.Join(mock.calls, "\n")
	if !strings.Contains(logStr, "comments add gt-wdr") {
		t.Errorf("expected a bead comment on gt-wdr recording the collapse; log:\n%s", logStr)
	}
	if strings.Contains(logStr, "comments add gt-ok") {
		t.Errorf("should not comment on the healthy issue gt-ok; log:\n%s", logStr)
	}
}

func TestDetectStateCollapse_SourceIssueStillOpen(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) == 0 {
				return "{}", nil
			}
			switch args[0] {
			case "list":
				return `[{"id":"gt-mr-1","status":"open","description":"branch: b\ntarget: main\nsource_issue: gt-open\n"}]`, nil
			case "show":
				return `[{"status":"in_progress"}]`, nil
			}
			return "{}", nil
		},
		func(args []string) error { return nil },
	)

	result := DetectStateCollapse(bd, "/work", "gastown", nil)
	if len(result.Findings) != 0 {
		t.Errorf("expected no findings when source issue is still open, got %+v", result.Findings)
	}
	if result.Checked != 1 {
		t.Errorf("Checked = %d, want 1", result.Checked)
	}
}

func TestDetectStateCollapse_SkipsMRsWithoutSourceIssue(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 0 && args[0] == "list" {
				return `[{"id":"gt-mr-1","status":"open","description":"no structured fields here"}]`, nil
			}
			return "{}", nil
		},
		func(args []string) error { return nil },
	)

	result := DetectStateCollapse(bd, "/work", "gastown", nil)
	if result.Checked != 0 || len(result.Findings) != 0 {
		t.Errorf("expected MR without source_issue to be skipped entirely, got %+v", result)
	}
}

// errFakeListFailure is a sentinel error used to simulate a bd list failure.
var errFakeListFailure = &fakeError{"simulated bd list failure"}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

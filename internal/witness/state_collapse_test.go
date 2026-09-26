package witness

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// recentTimestamp returns a timestamp inside the renotify window, for fixtures
// standing in for a report a previous patrol just wrote.
func recentTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func TestDetectStateCollapse_NilBd(t *testing.T) {
	t.Parallel()
	result := DetectStateCollapse(nil, fakeBranchRefSource(nil, nil), "/nonexistent", "testrig", nil)
	if result.Checked != 0 || len(result.Findings) != 0 || len(result.Errors) != 0 {
		t.Errorf("DetectStateCollapse(nil) = %+v, want empty result", result)
	}
}

// TestDetectStateCollapse_MRLookupUnavailable pins the honesty rule that the
// old plain-`bd list` implementation broke: the scan's entire subject matter
// arrives through the injected open-MR source, so without one there is
// nothing to examine and no finding may be claimed — MRLookupRan stays false
// and the error says the lookup was unavailable.
func TestDetectStateCollapse_MRLookupUnavailable(t *testing.T) {
	t.Parallel()

	noSource := fakeBranchRefSource(nil, nil)
	noSource.ListOpenMRs = nil

	for _, c := range []struct {
		name string
		refs *BranchRefSource
	}{
		{"nil refs", nil},
		{"refs with no ListOpenMRs", noSource},
	} {
		bd, mock := mockBd(func(args []string) (string, error) { return "[]", nil }, func(args []string) error { return nil })

		result := DetectStateCollapse(bd, c.refs, "/work", "testrig", nil)

		if result.MRLookupRan {
			t.Errorf("%s: MRLookupRan = true, want false", c.name)
		}
		if len(result.Errors) != 1 {
			t.Fatalf("%s: Errors = %d, want 1", c.name, len(result.Errors))
		}
		if !strings.Contains(result.Errors[0].Error(), "lookup unavailable") {
			t.Errorf("%s: error = %q, want it to say the lookup was unavailable", c.name, result.Errors[0].Error())
		}
		if result.Checked != 0 || len(result.Findings) != 0 {
			t.Errorf("%s: expected no findings without an MR source, got %+v", c.name, result)
		}
		if logStr := strings.Join(mock.calls, "\n"); logStr != "" {
			t.Errorf("%s: bd must not be consulted without an MR source; log:\n%s", c.name, logStr)
		}
	}
}

// TestDetectStateCollapse_MRLookupError is the other half of the same rule:
// the source exists but fails, so again nothing may be asserted.
func TestDetectStateCollapse_MRLookupError(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(func(args []string) (string, error) { return "[]", nil }, func(args []string) error { return nil })

	refs := fakeBranchRefSource(nil, nil)
	refs.ListOpenMRs = func() ([]OpenMRRef, error) { return nil, errFakeListFailure }

	result := DetectStateCollapse(bd, refs, "/work", "testrig", nil)

	if result.MRLookupRan {
		t.Error("MRLookupRan = true, want false")
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1", len(result.Errors))
	}
	if !strings.Contains(result.Errors[0].Error(), "lookup unavailable") {
		t.Errorf("error = %q, want it to say the lookup was unavailable", result.Errors[0].Error())
	}
	if result.Checked != 0 || len(result.Findings) != 0 {
		t.Errorf("expected no findings on lookup error, got %+v", result)
	}
	if logStr := strings.Join(mock.calls, "\n"); strings.Contains(logStr, "comments add") {
		t.Errorf("must not comment an unsupported finding; log:\n%s", logStr)
	}
}

func TestDetectStateCollapse_NoOpenMRs(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(func(args []string) (string, error) { return "[]", nil }, func(args []string) error { return nil })

	result := DetectStateCollapse(bd, fakeBranchRefSource(nil, nil), "/work", "testrig", nil)

	if !result.MRLookupRan {
		t.Error("MRLookupRan = false, want true")
	}
	if result.Checked != 0 || result.OpenMRsSeen != 0 || len(result.Findings) != 0 || len(result.Errors) != 0 {
		t.Errorf("expected empty result with an empty MR queue, got %+v", result)
	}
}

// TestDetectStateCollapse_ClosedSourceIssue is the core regression case: an
// open MR whose source issue is closed is the gt-zzd instance-4/6 signature
// (bead closed, MR open, fix not verified in force) and must be flagged.
func TestDetectStateCollapse_ClosedSourceIssue(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" {
				switch args[1] {
				case "gt-wdr":
					return `[{"status":"closed"}]`, nil
				case "gt-ok":
					return `[{"status":"open"}]`, nil
				}
				return "[]", nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSourceWithMRs(nil, nil, []OpenMRRef{
		{ID: "gt-mr-collapsed", Status: "open", SourceIssue: "gt-wdr", Branch: "polecat/topaz/gt-wdr+abc", Target: "main"},
		{ID: "gt-mr-healthy", Status: "open", SourceIssue: "gt-ok", Branch: "polecat/jade/gt-ok+def", Target: "main"},
	})

	result := DetectStateCollapse(bd, refs, "/work", "gastown", nil)

	if !result.MRLookupRan {
		t.Error("MRLookupRan = false, want true")
	}
	if result.OpenMRsSeen != 2 {
		t.Errorf("OpenMRsSeen = %d, want 2", result.OpenMRsSeen)
	}
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

// TestDetectStateCollapse_PendingMRInFlightNotFlagged is the false-positive
// class that un-blinding this detector exposed (gt-92ry): gt done closes the
// source issue as "pending_mr: <mr>" the moment it submits, so on the live
// rig 7 of 10 queued MRs presented as "closed issue, open MR" while being
// perfectly healthy submits. The named MR is in the queue, so the fix is in
// flight and there is no collapse.
func TestDetectStateCollapse_PendingMRInFlightNotFlagged(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-ipk7" {
				return `[{"status":"closed","close_reason":"pending_mr: gt-wisp-1c6"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSourceWithMRs(nil, nil, []OpenMRRef{
		{ID: "gt-wisp-1c6", Status: "open", SourceIssue: "gt-ipk7", Branch: "polecat/topaz/gt-ipk7+mu8jnw7r"},
	})

	result := DetectStateCollapse(bd, refs, "/work", "gastown", nil)

	if result.Checked != 1 {
		t.Errorf("Checked = %d, want 1 — suppression is a verdict, not a skip of candidacy", result.Checked)
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected no findings for a self-closed pending_mr issue, got %+v", result.Findings)
	}
	logStr := strings.Join(mock.calls, "\n")
	if strings.Contains(logStr, "comments add") {
		t.Errorf("should not comment on an issue whose MR is queued; log:\n%s", logStr)
	}
}

// TestDetectStateCollapse_PendingMRThatIsNotOpenStillFlagged is the guard the
// other way: the close reason is a claim about the queue, not the queue
// itself. An issue closed as pending_mr for an MR that is not open was
// closed with nothing in flight to land its fix.
func TestDetectStateCollapse_PendingMRThatIsNotOpenStillFlagged(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-tlwv" {
				return `[{"status":"closed","close_reason":"pending_mr: gt-wisp-jww"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	// gt-tlwv's queued MR is gt-wisp-live, but the close reason claims
	// gt-wisp-jww, which is not in the queue at all.
	refs := fakeBranchRefSourceWithMRs(nil, nil, []OpenMRRef{
		{ID: "gt-wisp-live", Status: "open", SourceIssue: "gt-tlwv", Branch: "polecat/quartz/gt-tlwv+mu5a11bc"},
	})

	result := DetectStateCollapse(bd, refs, "/work", "gastown", nil)

	if len(result.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1: %+v", len(result.Findings), result.Findings)
	}
	if result.Findings[0].IssueID != "gt-tlwv" {
		t.Errorf("IssueID = %q, want gt-tlwv", result.Findings[0].IssueID)
	}
}

// TestDetectStateCollapse_SourceIssueStillOpen is the guard the other way: a
// source issue that has not closed is not a collapse, however open its MR is.
func TestDetectStateCollapse_SourceIssueStillOpen(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" {
				return `[{"status":"in_progress"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSourceWithMRs(nil, nil, []OpenMRRef{
		{ID: "gt-mr-1", Status: "open", SourceIssue: "gt-open", Branch: "b", Target: "main"},
	})
	result := DetectStateCollapse(bd, refs, "/work", "gastown", nil)

	if len(result.Findings) != 0 {
		t.Errorf("expected no findings when source issue is still open, got %+v", result.Findings)
	}
	if result.Checked != 1 {
		t.Errorf("Checked = %d, want 1", result.Checked)
	}
}

func TestDetectStateCollapse_SkipsMRsWithoutSourceIssue(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(func(args []string) (string, error) { return "[]", nil }, func(args []string) error { return nil })

	refs := fakeBranchRefSourceWithMRs(nil, nil, []OpenMRRef{
		{ID: "gt-mr-1", Status: "open"}, // no SourceIssue recorded
	})

	result := DetectStateCollapse(bd, refs, "/work", "gastown", nil)
	if result.Checked != 0 || len(result.Findings) != 0 {
		t.Errorf("expected MR without source_issue to be skipped entirely, got %+v", result)
	}
}

// TestDetectStateCollapse_SeesWispsInvisibleToBdList is the gt-92ry
// regression proper. Merge-request beads are wisps (gt mq submit, GH#2446),
// which `bd list --label=gt:merge-request` does not return: the shipped
// detector read the queue that way, so on a rig whose entire queue is wisps
// it saw zero MRs and printed "No state collapse found (0 open MRs)" — a
// failure serialised as success. bd is mocked here to answer every list call
// with an empty array, exactly as it did in the field; the injected source
// returns the two MRs that were really there, and both collapses must be
// reported.
func TestDetectStateCollapse_SeesWispsInvisibleToBdList(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 0 && args[0] == "list" {
				return "[]", nil // what the wisp-blind `bd list` returned
			}
			if len(args) > 1 && args[0] == "show" {
				return `[{"status":"closed"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSourceWithMRs(nil, nil, []OpenMRRef{
		{ID: "gt-wisp-aaaa", Status: "open", SourceIssue: "gt-closed-1", Branch: "polecat/a/gt-closed-1+m1"},
		{ID: "gt-wisp-bbbb", Status: "open", SourceIssue: "gt-closed-2", Branch: "polecat/b/gt-closed-2+m2"},
	})

	result := DetectStateCollapse(bd, refs, "/work", "gastown", nil)

	if !result.MRLookupRan {
		t.Fatal("MRLookupRan = false, want true — the injected source is the whole point")
	}
	if len(result.Findings) != 2 {
		t.Fatalf("Findings = %d, want 2 (both wisps are collapses): %+v", len(result.Findings), result.Findings)
	}
	if got := []string{result.Findings[0].MRID, result.Findings[1].MRID}; got[0] != "gt-wisp-aaaa" || got[1] != "gt-wisp-bbbb" {
		t.Errorf("finding MR ids = %v, want the two wisp ids", got)
	}

	// The queue must not be re-read through the wisp-blind path.
	logStr := strings.Join(mock.calls, "\n")
	if strings.Contains(logStr, "--label=gt:merge-request") {
		t.Errorf("the MR queue must not be read via `bd list --label=gt:merge-request`; log:\n%s", logStr)
	}
}

func TestStateCollapseSummary_AllClearStatesTheLookupRan(t *testing.T) {
	t.Parallel()
	mr := &DetectStateCollapseResult{Checked: 3, MRLookupRan: true, OpenMRsSeen: 3}
	branches := &DetectStrandedBranchesResult{Checked: 5, MRLookupRan: true, OpenMRsSeen: 3}

	summary, allClear := StateCollapseSummary(mr, branches, "gastown")

	if !allClear {
		t.Errorf("allClear = false, want true for a scan that ran and found nothing: %q", summary)
	}
	if !strings.Contains(summary, "No state collapse found") {
		t.Errorf("summary = %q, want the all-clear wording", summary)
	}
	if !strings.Contains(summary, "checked 3 open MR(s)") || !strings.Contains(summary, "lookup ran") {
		t.Errorf("summary = %q, want it to name the check count and that the lookup ran", summary)
	}
}

// TestStateCollapseSummary_UnresolvedLookupIsNotAllClear is the wording half
// of gt-92ry: with no resolved MR queue the scan examined nothing, and
// "no state collapse found" must never be printed for it — that string is
// what made an unchecked scan indistinguishable from a checked-clean one.
func TestStateCollapseSummary_UnresolvedLookupIsNotAllClear(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		mrRan    bool
		branchOK bool
	}{
		{"neither lookup resolved", false, false},
		{"MR-driven check never ran", false, true},
		{"branch-driven check never ran", true, false},
	}
	for _, c := range cases {
		mr := &DetectStateCollapseResult{MRLookupRan: c.mrRan}
		branches := &DetectStrandedBranchesResult{MRLookupRan: c.branchOK}

		summary, allClear := StateCollapseSummary(mr, branches, "gastown")

		if allClear {
			t.Errorf("%s: allClear = true, want false", c.name)
		}
		if strings.Contains(summary, "No state collapse found") {
			t.Errorf("%s: summary = %q — must never print the all-clear wording when a lookup did not resolve", c.name, summary)
		}
		if !strings.Contains(summary, "lookup unavailable") {
			t.Errorf("%s: summary = %q, want it to say the lookup was unavailable", c.name, summary)
		}
	}
}

func TestStateCollapseSummary_FindingsReported(t *testing.T) {
	t.Parallel()
	mr := &DetectStateCollapseResult{
		MRLookupRan: true,
		OpenMRsSeen: 2,
		Findings:    []StateCollapseFinding{{IssueID: "gt-a"}, {IssueID: "gt-b"}},
	}
	branches := &DetectStrandedBranchesResult{MRLookupRan: true, Checked: 4}

	summary, allClear := StateCollapseSummary(mr, branches, "gastown")

	if allClear {
		t.Error("allClear = true, want false with findings present")
	}
	if !strings.Contains(summary, "Found 2 state collapse(s)") {
		t.Errorf("summary = %q, want the finding count", summary)
	}
}

// TestStateCollapseSummary_NamesSuppressions pins that a suppressed branch is
// a stated fact, not a silent drop: "0 findings" and "0 findings, 1 branch
// adjudicated" are different claims about the rig (gt-hsum).
func TestStateCollapseSummary_NamesSuppressions(t *testing.T) {
	t.Parallel()
	mr := &DetectStateCollapseResult{MRLookupRan: true, Checked: 2}
	branches := &DetectStrandedBranchesResult{
		MRLookupRan: true,
		Checked:     1,
		Superseded:  []SupersededBranch{{IssueID: "om-i0p", Branch: "polecat/opal/om-i0p+mu9vzh49"}},
	}

	summary, allClear := StateCollapseSummary(mr, branches, "om")

	if !allClear {
		t.Errorf("allClear = false, want true — suppressions are not findings: %q", summary)
	}
	if !strings.Contains(summary, "1 superseded branch(es) suppressed") {
		t.Errorf("summary = %q, want the suppression count", summary)
	}

	// The same count must survive alongside a real finding.
	mr.Findings = []StateCollapseFinding{{IssueID: "om-a"}}
	summary, allClear = StateCollapseSummary(mr, branches, "om")
	if allClear {
		t.Error("allClear = true, want false with a finding present")
	}
	if !strings.Contains(summary, "1 superseded branch(es) suppressed") {
		t.Errorf("summary = %q, want the suppression count beside the finding count", summary)
	}
}

func TestStateCollapseSummary_NilResults(t *testing.T) {
	t.Parallel()

	summary, allClear := StateCollapseSummary(nil, nil, "gastown")

	if allClear {
		t.Error("allClear = true with no results at all, want false")
	}
	if strings.Contains(summary, "No state collapse found") {
		t.Errorf("summary = %q — absent results must not read as a clean scan", summary)
	}
}

// errFakeListFailure is a sentinel error used to simulate a bd list failure.
var errFakeListFailure = &fakeError{"simulated bd list failure"}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

// fakeBranchRefSource builds a BranchRefSource from static test fixtures:
// branches on the remote, and which issue ids have a referencing commit on
// the target branch. The open MR queue is empty — tests that need a queued
// MR use fakeBranchRefSourceWithMRs.
func fakeBranchRefSource(branches []string, referenced map[string]bool) *BranchRefSource {
	return fakeBranchRefSourceWithMRs(branches, referenced, nil)
}

// fakeBranchRefSourceWithMRs adds a fixed set of open merge requests, the
// state the scan must consult before calling a branch stranded (gt-akap).
func fakeBranchRefSourceWithMRs(branches []string, referenced map[string]bool, openMRs []OpenMRRef) *BranchRefSource {
	return &BranchRefSource{
		ListPolecatBranches: func() ([]string, error) {
			return branches, nil
		},
		TargetHasCommitReferencing: func(target, issueID string) (bool, error) {
			return referenced[issueID], nil
		},
		ListOpenMRs: func() ([]OpenMRRef, error) {
			return openMRs, nil
		},
	}
}

func TestDetectStrandedBranches_NilArgs(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(func(args []string) (string, error) { return "[]", nil }, func(args []string) error { return nil })

	if r := DetectStrandedBranches(nil, fakeBranchRefSource(nil, nil), "/work", "gastown", "main", nil); r.Checked != 0 || len(r.Findings) != 0 {
		t.Errorf("nil bd: got %+v, want empty", r)
	}
	if r := DetectStrandedBranches(bd, nil, "/work", "gastown", "main", nil); r.Checked != 0 || len(r.Findings) != 0 {
		t.Errorf("nil refs: got %+v, want empty", r)
	}
}

// TestDetectStrandedBranches_GenuineStrand is the core regression case
// (gt-3ii/gt-hsg): a bead is closed, its branch was pushed, no commit on the
// target branch references it, and the close reason is not a deliberate
// discard. This must be flagged — the detector exists to catch exactly this.
func TestDetectStrandedBranches_GenuineStrand(t *testing.T) {
	t.Parallel()
	branch := "polecat/pyrite/gt-hsg+mttuo7sf"
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-hsg" {
				return `[{"status":"closed","close_reason":"fixed the fail-open destruction gate"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{branch}, nil) // no issue referenced anywhere on target
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if result.Checked != 1 {
		t.Errorf("Checked = %d, want 1", result.Checked)
	}
	if len(result.Errors) != 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1: %+v", len(result.Findings), result.Findings)
	}
	f := result.Findings[0]
	if f.IssueID != "gt-hsg" {
		t.Errorf("IssueID = %q, want gt-hsg", f.IssueID)
	}
	if f.Branch != branch {
		t.Errorf("Branch = %q, want %q", f.Branch, branch)
	}

	logStr := strings.Join(mock.calls, "\n")
	if !strings.Contains(logStr, "comments add gt-hsg") {
		t.Errorf("expected a bead comment on gt-hsg recording the strand; log:\n%s", logStr)
	}
}

// TestDetectStrandedBranches_SquashMergedNotFlagged is the false-positive
// class that killed the first proposed detector shape (gt-3ii comment
// history: ancestry flagged 12/12 unmerged branches, 12/12 of them squash
// merges that had actually landed). A closed bead whose issue id shows up in
// a target-branch commit message must NOT be flagged even though the branch
// tip is not (and never will be) an ancestor.
func TestDetectStrandedBranches_SquashMergedNotFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/topaz/gt-wdr+abc"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-wdr" {
				return `[{"status":"closed","close_reason":"merged via refinery"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{branch}, map[string]bool{"gt-wdr": true})
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if result.Checked != 1 {
		t.Errorf("Checked = %d, want 1", result.Checked)
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected no findings for a squash-merged branch, got %+v", result.Findings)
	}
}

// TestDetectStrandedBranches_DeliberateDiscardNotFlagged is the second
// false-positive class (gt-3ii, gt-g6b): a polecat that finds its fix
// already landed under a different issue closes with the documented
// "no-changes:" convention and abandons its branch rather than pushing a
// duplicate MR. That leaves the same on-disk signature as a genuine strand
// and must not be flagged.
func TestDetectStrandedBranches_DeliberateDiscardNotFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/basalt/gt-g6b+xyz"
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-g6b" {
				return `[{"status":"closed","close_reason":"no-changes: already fixed upstream by gt-80o, discarding this branch"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{branch}, nil) // no referencing commit either — same shape as a strand
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if len(result.Findings) != 0 {
		t.Errorf("expected no findings for a deliberate discard, got %+v", result.Findings)
	}
	// The closed-issue branch was still examined (Checked counts examination,
	// not candidacy) — it's the discard filter that keeps it out of Findings.
	if result.Checked != 1 {
		t.Errorf("Checked = %d, want 1", result.Checked)
	}
	logStr := strings.Join(mock.calls, "\n")
	if strings.Contains(logStr, "comments add gt-g6b") {
		t.Errorf("should not comment on a deliberately discarded issue; log:\n%s", logStr)
	}
}

func TestDetectStrandedBranches_OpenIssueNotFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/jade/gt-open+abc"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-open" {
				return `[{"status":"in_progress"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{branch}, nil)
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if result.Checked != 0 || len(result.Findings) != 0 {
		t.Errorf("expected an in-progress issue's branch to be skipped entirely, got %+v", result)
	}
}

func TestDetectStrandedBranches_SkipsBranchesWithoutIssueID(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(
		func(args []string) (string, error) { return "[]", nil },
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{"polecat/jade-adhoc"}, nil)
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if result.Checked != 0 || len(result.Findings) != 0 {
		t.Errorf("expected a branch with no encoded issue id to be skipped, got %+v", result)
	}
}

func TestDetectStrandedBranches_ListError(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(func(args []string) (string, error) { return "[]", nil }, func(args []string) error { return nil })
	refs := &BranchRefSource{
		ListPolecatBranches: func() ([]string, error) { return nil, errFakeListFailure },
		TargetHasCommitReferencing: func(target, issueID string) (bool, error) {
			return false, nil
		},
		ListOpenMRs: func() ([]OpenMRRef, error) { return nil, nil },
	}

	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1", len(result.Errors))
	}
	if result.Checked != 0 || len(result.Findings) != 0 {
		t.Errorf("expected no findings on list error, got %+v", result)
	}
}

// TestDetectStrandedBranches_OpenMRForSourceIssueNotFlagged is the core
// regression for gt-akap: the live scan reported "has no MR" for 11 of 13
// findings, every one of them an MR sitting open in the queue. A closed
// issue whose MR is queued is the ordinary polecat self-close workflow and
// must not be reported as a strand.
func TestDetectStrandedBranches_OpenMRForSourceIssueNotFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/coral/gt-off9+mu7h074n"
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-off9" {
				return `[{"status":"closed","close_reason":"pending_mr: gt-wisp-6okk"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSourceWithMRs([]string{branch}, nil, []OpenMRRef{
		{ID: "gt-wisp-6okk", SourceIssue: "gt-off9", Branch: branch},
	})
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if !result.MRLookupRan {
		t.Error("MRLookupRan = false, want true")
	}
	if result.OpenMRsSeen != 1 {
		t.Errorf("OpenMRsSeen = %d, want 1", result.OpenMRsSeen)
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected no findings for an issue with an open MR, got %+v", result.Findings)
	}
	// The closed-issue branch was still examined; suppression is a verdict,
	// not a skip of candidacy.
	if result.Checked != 1 {
		t.Errorf("Checked = %d, want 1", result.Checked)
	}
	logStr := strings.Join(mock.calls, "\n")
	if strings.Contains(logStr, "comments add gt-off9") {
		t.Errorf("should not comment on an issue whose MR is queued; log:\n%s", logStr)
	}
}

// TestDetectStrandedBranches_OpenMRByBranchOnlyNotFlagged covers the MR
// whose source_issue field is absent or names a different bead: the branch
// it will land is still unambiguous, so it must suppress the finding too.
func TestDetectStrandedBranches_OpenMRByBranchOnlyNotFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/jasper/gt-624w+mu6t81zz"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-624w" {
				return `[{"status":"closed","close_reason":"pending_mr: gt-wisp-33p"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSourceWithMRs([]string{branch}, nil, []OpenMRRef{
		{ID: "gt-wisp-33p", Branch: branch}, // no SourceIssue recorded
	})
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if len(result.Findings) != 0 {
		t.Errorf("expected no findings when an open MR carries the branch, got %+v", result.Findings)
	}
}

// TestDetectStrandedBranches_OpenMRByPendingCloseReasonNotFlagged covers the
// gh-akap workflow shape directly: the close reason names the MR, and that
// MR is open. Even if neither the source_issue nor the branch fields match,
// the named MR is in the queue and the fix is in flight.
func TestDetectStrandedBranches_OpenMRByPendingCloseReasonNotFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/topaz/gt-wdr+abc"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-wdr" {
				return `[{"status":"closed","close_reason":"pending_mr: gt-wisp-tmki"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSourceWithMRs([]string{branch}, nil, []OpenMRRef{
		{ID: "gt-wisp-tmki"}, // neither field recorded — only the reason names it
	})
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if len(result.Findings) != 0 {
		t.Errorf("expected no findings when the close reason names an open MR, got %+v", result.Findings)
	}
}

// supersededNotes is the merge-rejection record the refinery writes into a
// source bead's notes, as it appears on the live om-i0p bead (gt-hsum): the
// marker, the reviewer's prose, then the structured trailer naming the
// rejected branch.
const supersededNotes = `MERGE REJECTION (attempt 1): editorial - REJECTED: fail-open regression in a security boundary.

The word-boundary regex drops names where "claude" abuts an alphanumeric.

Reopening om-i0p for redispatch. The rejected head is d384225.
Branch: polecat/opal/om-i0p+mu9vzh49
Target: main
MR: [deleted:om-wisp-loh]`

// TestDetectStrandedBranches_SupersededAttemptNotFlagged is the gt-hsum
// regression: the live om-i0p shape, where the flagged branch is the issue's
// rejected first attempt. Its rework landed under a different branch, so
// nothing on target references the issue and the landing MR was purged —
// git state alone cannot distinguish it from a strand. The issue's own
// record can, and it must not be re-flagged every patrol cycle.
func TestDetectStrandedBranches_SupersededAttemptNotFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/opal/om-i0p+mu9vzh49"
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "om-i0p" {
				return `[{"status":"closed","close_reason":"pending_mr: om-wisp-wlw (attempt 1)","notes":` +
					jsonString(supersededNotes) + `}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	// om-wisp-wlw is absent from the queue — the landing MR was purged after
	// merging, which is what made this recur. The closure attests attempt 1,
	// the same attempt the note rejects, so the pairing holds (gt-0cp3).
	refs := fakeBranchRefSourceWithMRs([]string{branch}, nil, []OpenMRRef{
		{ID: "om-wisp-unrelated", SourceIssue: "om-something-else"},
	})
	result := DetectStrandedBranches(bd, refs, "/work", "om", "main", nil)

	if len(result.Findings) != 0 {
		t.Errorf("expected no finding for an adjudicated rejected attempt, got %+v", result.Findings)
	}
	if len(result.Superseded) != 1 {
		t.Fatalf("Superseded = %d, want 1: %+v", len(result.Superseded), result.Superseded)
	}
	if got := result.Superseded[0]; got.IssueID != "om-i0p" || got.Branch != branch || got.MRID != "om-wisp-wlw" {
		t.Errorf("Superseded[0] = %+v, want om-i0p / %s / om-wisp-wlw", got, branch)
	}
	if logStr := strings.Join(mock.calls, "\n"); strings.Contains(logStr, "comments add om-i0p") {
		t.Errorf("a suppressed candidate must not be commented; log:\n%s", logStr)
	}
}

// TestDetectStrandedBranches_CloseReasonSupersessionVerifiedNotFlagged is the
// gt-xpro regression: the live gt-nj23.7 shape, where a sub-issue in a series
// is closed by hand naming the later sub-issue that supersedes it directly in
// the close reason — "Superseded by <id>: ...", no pending_mr prefix and no
// rejection note. supersededByRecord cannot see this shape (neither half of
// its check matches), and the later issue's landing commit carries only its
// own token, so the target grep for THIS issue also comes up empty. Both
// paths that would normally save a superseded candidate miss it; this is the
// finding gt-xpro reported as a false positive.
func TestDetectStrandedBranches_CloseReasonSupersessionVerifiedNotFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/pearl/gt-nj23.7+mu8pdbpp"
	closeReason := "Superseded by gt-nj23.8 (T8): strict superset (T7 content + daemon.go " +
		"integration, 168 insertions over T7). T8 rework is in-flight (MR gt-wisp-buy, " +
		"ready in queue). Do not re-dispatch T7 separately."
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-nj23.7" {
				return `[{"status":"closed","close_reason":` + jsonString(closeReason) + `}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	// gt-nj23.8's squash commit is on the target, carrying only its own
	// token — gt-nj23.7 itself is not referenced anywhere on target.
	refs := fakeBranchRefSource([]string{branch}, map[string]bool{"gt-nj23.8": true})
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if len(result.Findings) != 0 {
		t.Errorf("expected no finding for a verified close-reason supersession, got %+v", result.Findings)
	}
	if len(result.Superseded) != 1 {
		t.Fatalf("Superseded = %d, want 1: %+v", len(result.Superseded), result.Superseded)
	}
	if got := result.Superseded[0]; got.IssueID != "gt-nj23.7" || got.Branch != branch || got.MRID != "gt-nj23.8" {
		t.Errorf("Superseded[0] = %+v, want gt-nj23.7 / %s / gt-nj23.8", got, branch)
	}
	if logStr := strings.Join(mock.calls, "\n"); strings.Contains(logStr, "comments add gt-nj23.7") {
		t.Errorf("a suppressed candidate must not be commented; log:\n%s", logStr)
	}
}

// TestDetectStrandedBranches_CloseReasonSupersessionUnverifiedStillFlagged
// pins the other half: naming a successor in the close reason is a claim,
// not proof. If the named issue's fix is not (yet) verifiable on the target,
// the candidate must still be reported rather than suppressed on the claim
// alone.
func TestDetectStrandedBranches_CloseReasonSupersessionUnverifiedStillFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/pearl/gt-nj23.7+mu8pdbpp"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-nj23.7" {
				return `[{"status":"closed","close_reason":"Superseded by gt-nj23.8: rework in flight"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	// Neither gt-nj23.7 nor gt-nj23.8 is referenced on target yet.
	refs := fakeBranchRefSource([]string{branch}, nil)
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if len(result.Superseded) != 0 {
		t.Errorf("Superseded = %+v, want none — the successor's fix is not verified in force", result.Superseded)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1: %+v", len(result.Findings), result.Findings)
	}
}

func TestSupersededIssueFromCloseReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		closeReason string
		want        string
	}{
		{"live gt-nj23.7 shape", "Superseded by gt-nj23.8 (T8): strict superset, 168 insertions over T7.", "gt-nj23.8"},
		{"case insensitive marker", "superseded by GT-ABC: folded in", "GT-ABC"},
		{"trailing period only", "Fixed elsewhere. Superseded by gt-abc.", "gt-abc"},
		{"colon immediately after id", "Superseded by gt-abc: see thread", "gt-abc"},
		{"comma immediately after id", "Superseded by gt-abc, landed there instead", "gt-abc"},
		{"no marker", "fixed, closing by hand", ""},
		{"empty reason", "", ""},
		{"literal Closed", "Closed", ""},
		{"pending_mr shape is not this shape", "pending_mr: gt-wisp-a9j", ""},
		{"marker with nothing after it", "Superseded by", ""},
		{"marker with only whitespace after it", "Superseded by   ", ""},
	}
	for _, c := range cases {
		if got := supersededIssueFromCloseReason(c.closeReason); got != c.want {
			t.Errorf("%s: supersededIssueFromCloseReason(%q) = %q, want %q", c.name, c.closeReason, got, c.want)
		}
	}
}

// TestDetectStrandedBranches_RejectionOfAnotherBranchStillFlagged keeps the
// suppression branch-scoped: a rejection record that names a different branch
// explains nothing about this one, however well the attempts line up.
func TestDetectStrandedBranches_RejectionOfAnotherBranchStillFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/opal/om-i0p+mu9vzh49"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "om-i0p" {
				return `[{"status":"closed","close_reason":"pending_mr: om-wisp-wlw (attempt 1)","notes":` +
					jsonString(strings.Replace(supersededNotes, branch, "polecat/opal/om-i0p+othereattempt", 1)) + `}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{branch}, nil)
	result := DetectStrandedBranches(bd, refs, "/work", "om", "main", nil)

	if len(result.Superseded) != 0 {
		t.Errorf("Superseded = %+v, want none — the record names a different branch", result.Superseded)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1: %+v", len(result.Findings), result.Findings)
	}
}

// TestDetectStrandedBranches_RejectionWithoutQueueClosureStillFlagged pins the
// other half of the rule: a recorded rejection alone does not suppress. A
// rejected attempt whose issue is closed for any other reason can still be a
// collapse — the retry never entered the queue — so it stays reportable.
func TestDetectStrandedBranches_RejectionWithoutQueueClosureStillFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/opal/om-i0p+mu9vzh49"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "om-i0p" {
				return `[{"status":"closed","close_reason":"fixed, closing by hand","notes":` +
					jsonString(supersededNotes) + `}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{branch}, nil)
	result := DetectStrandedBranches(bd, refs, "/work", "om", "main", nil)

	if len(result.Superseded) != 0 {
		t.Errorf("Superseded = %+v, want none — the closure never re-entered the queue", result.Superseded)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1: %+v", len(result.Findings), result.Findings)
	}
}

func TestRejectedBranchAttemptFromNotes(t *testing.T) {
	t.Parallel()
	branch := "polecat/opal/om-i0p+mu9vzh49"

	secondAttempt := supersededNotes + `

MERGE REJECTION (attempt 2): tests - FAILED: gate red.
Branch: polecat/opal/om-i0p+secondattempt
Target: main
MR: om-wisp-xyz`

	bothAttempts := supersededNotes + `

MERGE REJECTION (attempt 2): tests - FAILED: gate red.
Branch: ` + branch + `
Target: main
MR: om-wisp-xyz`

	cases := []struct {
		name   string
		notes  string
		branch string
		want   int
		wantOK bool
	}{
		{"record naming the branch", supersededNotes, branch, 1, true},
		{"refs/heads prefix on either side", supersededNotes, "refs/heads/" + branch, 1, true},
		{"padding around the value", strings.Replace(supersededNotes, "Branch: ", "branch:   ", 1), branch, 1, true},
		{"later attempt names another branch", secondAttempt, "polecat/opal/om-i0p+secondattempt", 2, true},
		{"record for another branch", supersededNotes, "polecat/opal/om-other+xyz", 0, false},
		{"no rejection record", "just some notes about the fix", branch, 0, false},
		{"marker without a branch line", "MERGE REJECTION (attempt 1): see thread above", branch, 0, false},
		{"branch named only in prose", "MERGE REJECTION (attempt 1): unlike " + branch +
			" this one kept the probe\ntarget: main", branch, 0, false},
		{"empty notes", "", branch, 0, false},
		{"newest rejection for the branch wins", bothAttempts, branch, 2, true},
	}
	for _, c := range cases {
		attempt, ok := rejectedBranchAttemptFromNotes(c.notes, c.branch)
		if got := ok; got != c.wantOK {
			t.Errorf("%s: rejectedBranchAttemptFromNotes(...) ok = %v, want %v", c.name, got, c.wantOK)
		}
		if got := attempt; got != c.want {
			t.Errorf("%s: rejectedBranchAttemptFromNotes(...) = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestDetectStrandedBranches_OlderRejectionDoesNotHideNewerAttempt is the
// gt-0cp3 defect: the polecat branch is reused across every attempt of an
// issue, so attempt 1's rejection note names the same branch as a later
// attempt. The closure gt done writes for the later attempt names a HIGHER
// attempt number; pairing the closure with only the older rejection then
// suppresses a branch that was never rejected — a genuine strand read as a
// superseded attempt. The newest rejection the closure's attempt reached is
// the one that adjudicates it, and when the closure outruns every recorded
// rejection the branch stays reportable.
func TestDetectStrandedBranches_OlderRejectionDoesNotHideNewerAttempt(t *testing.T) {
	t.Parallel()
	branch := "polecat/opal/om-i0p+mu9vzh49"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "om-i0p" {
				return `[{"status":"closed","close_reason":"pending_mr: om-wisp-wlw (attempt 2)","notes":` +
					jsonString(supersededNotes) + `}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{branch}, nil)
	result := DetectStrandedBranches(bd, refs, "/work", "om", "main", nil)

	if len(result.Superseded) != 0 {
		t.Errorf("Superseded = %+v, want none — attempt 2's closure outruns attempt 1's rejection", result.Superseded)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1: %+v", len(result.Findings), result.Findings)
	}
}

// TestDetectStrandedBranches_NewerRejectionSupersedesTheClosure is the
// complementary gt-0cp3 case: the refinery reopens the issue on rejection and
// the polecat's transient self-close writes the closure BEFORE the refinery
// appends the rejection note for that same attempt, so the issue ends closed
// with a rejection note on an attempt the closure itself attested. The
// rejection lands by the time of the scan, and it is the one that adjudicates
// the closure — the pairing holds on the newest rejection, not the first.
func TestDetectStrandedBranches_NewerRejectionSupersedesTheClosure(t *testing.T) {
	t.Parallel()
	branch := "polecat/opal/om-i0p+mu9vzh49"
	bothNotes := supersededNotes + `

MERGE REJECTION (attempt 2): editorial - REJECTED: still fails the boundary probe.
Branch: ` + branch + `
Target: main
MR: om-wisp-second`
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "om-i0p" {
				return `[{"status":"closed","close_reason":"pending_mr: om-wisp-wlw (attempt 2)","notes":` +
					jsonString(bothNotes) + `}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	// The landing MRs are both absent from the queue: attempt 1's was purged
	// after merging, attempt 2's was rejected — exactly the state where the
	// record, not git, is the only adjudication.
	refs := fakeBranchRefSourceWithMRs([]string{branch}, nil, []OpenMRRef{
		{ID: "om-wisp-unrelated", SourceIssue: "om-something-else"},
	})
	result := DetectStrandedBranches(bd, refs, "/work", "om", "main", nil)

	if len(result.Findings) != 0 {
		t.Errorf("expected no finding when the closure's own attempt is the rejected one, got %+v", result.Findings)
	}
	if len(result.Superseded) != 1 {
		t.Fatalf("Superseded = %d, want 1: %+v", len(result.Superseded), result.Superseded)
	}
}

// TestDetectStrandedBranches_LegacyClosureShapeStillFlagged: a pending_mr
// closure written before the attempt suffix exists attests to no attempt. It
// cannot pair with any rejection — not even one it postdates — so the branch
// stays reportable rather than suppressed on an unverifiable pairing
// (gt-0cp3).
func TestDetectStrandedBranches_LegacyClosureShapeStillFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/opal/om-i0p+mu9vzh49"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "om-i0p" {
				return `[{"status":"closed","close_reason":"pending_mr: om-wisp-wlw","notes":` +
					jsonString(supersededNotes) + `}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{branch}, nil)
	result := DetectStrandedBranches(bd, refs, "/work", "om", "main", nil)

	if len(result.Superseded) != 0 {
		t.Errorf("Superseded = %+v, want none — the closure attests no attempt", result.Superseded)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1: %+v", len(result.Findings), result.Findings)
	}
}

// jsonString renders s as a JSON string literal, so a test fixture can embed
// multi-line bead notes in the `bd show --json` payload a mock returns.
func jsonString(s string) string {
	quoted, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(quoted)
}

// TestDetectStrandedBranches_ClosedMRSameSourceIssueStillFlagged is the
// guard the other way: a close reason is a claim about the queue, not the
// queue itself. A source issue closed as pending_mr for an MR that is NOT
// open (purged, or closed without merging) is still a strand candidate and
// must be reported.
func TestDetectStrandedBranches_ClosedMRSameSourceIssueStillFlagged(t *testing.T) {
	t.Parallel()
	branch := "polecat/quartz/gt-tlwv+mu5a11bc"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-tlwv" {
				return `[{"status":"closed","close_reason":"pending_mr: gt-wisp-jww"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	// gt-wisp-jww is absent from the open queue (the live gt-tlwv case).
	refs := fakeBranchRefSourceWithMRs([]string{branch}, nil, []OpenMRRef{
		{ID: "gt-wisp-unrelated", SourceIssue: "gt-something-else"},
	})
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if len(result.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1: %+v", len(result.Findings), result.Findings)
	}
	if result.Findings[0].IssueID != "gt-tlwv" {
		t.Errorf("IssueID = %q, want gt-tlwv", result.Findings[0].IssueID)
	}
}

// TestDetectStrandedBranches_MRLookupUnavailable pins the honesty rule: the
// finding asserts "no MR", so a scan that never resolved the queue must
// produce no findings at all rather than findings it cannot support.
func TestDetectStrandedBranches_MRLookupUnavailable(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-hsg" {
				return `[{"status":"closed","close_reason":"fixed the gate"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{"polecat/pyrite/gt-hsg+mttuo7sf"}, nil)
	refs.ListOpenMRs = nil

	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if result.MRLookupRan {
		t.Error("MRLookupRan = true, want false")
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1", len(result.Errors))
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected no findings without an MR lookup, got %+v", result.Findings)
	}
	logStr := strings.Join(mock.calls, "\n")
	if strings.Contains(logStr, "comments add gt-hsg") {
		t.Errorf("must not comment an unsupported strand; log:\n%s", logStr)
	}
}

// TestDetectStrandedBranches_MRLookupError is the other half: the lookup
// exists but fails, so again nothing may be asserted.
func TestDetectStrandedBranches_MRLookupError(t *testing.T) {
	t.Parallel()
	bd, mock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-hsg" {
				return `[{"status":"closed","close_reason":"fixed the gate"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{"polecat/pyrite/gt-hsg+mttuo7sf"}, nil)
	refs.ListOpenMRs = func() ([]OpenMRRef, error) { return nil, errFakeListFailure }

	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if result.MRLookupRan {
		t.Error("MRLookupRan = true, want false")
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1", len(result.Errors))
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected no findings when the MR lookup fails, got %+v", result.Findings)
	}
	logStr := strings.Join(mock.calls, "\n")
	if strings.Contains(logStr, "comments add gt-hsg") {
		t.Errorf("must not comment an unsupported strand; log:\n%s", logStr)
	}
}

func TestOpenMRSetCovers(t *testing.T) {
	t.Parallel()
	set := NewOpenMRSet([]OpenMRRef{
		{ID: "gt-wisp-1", SourceIssue: "gt-a", Branch: "polecat/x/gt-a+1"},
		{ID: "gt-wisp-2", Branch: "refs/heads/polecat/y/gt-b+2"}, // prefix + whitespace tolerated
		{ID: "gt-wisp-3"},
		{ID: "  "}, // unreferenceable, dropped
	})

	cases := []struct {
		name        string
		issue       string
		branch      string
		closeReason string
		want        string
	}{
		{"by source issue", "gt-a", "", "", "gt-wisp-1"},
		{"by branch", "", "polecat/x/gt-a+1", "", "gt-wisp-1"},
		{"branch with refs/heads prefix", "", "polecat/y/gt-b+2", "", "gt-wisp-2"},
		{"by pending_mr close reason", "gt-z", "polecat/z/gt-z+3", "pending_mr: gt-wisp-3", "gt-wisp-3"},
		{"pending_mr naming a closed/absent MR does not cover", "gt-z", "polecat/z/gt-z+3", "pending_mr: gt-wisp-99", ""},
		{"unrelated", "gt-z", "polecat/z/gt-z+3", "pending_mr: gt-wisp-3 extra prose", ""},
		{"unrelated even with prose reason", "gt-z", "polecat/z/gt-z+3", "no-changes: covered elsewhere", ""},
	}
	for _, c := range cases {
		if got := set.covers(c.issue, c.branch, c.closeReason); got != c.want {
			t.Errorf("%s: covers(%q, %q, %q) = %q, want %q",
				c.name, c.issue, c.branch, c.closeReason, got, c.want)
		}
	}

	var nilSet *OpenMRSet
	if got := nilSet.covers("gt-a", "polecat/x/gt-a+1", "pending_mr: gt-wisp-1"); got != "" {
		t.Errorf("nil set covers = %q, want empty", got)
	}
}

func TestIsDeliberateDiscard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		reason string
		want   bool
	}{
		{"no-changes: already fixed upstream", true},
		{"  No-Changes: case-insensitive and padded", true},
		{"merged cleanly", false},
		{"", false},
		{"fixed by rewriting the no-changes detector", false}, // must not match mid-string
	}
	for _, c := range cases {
		if got := isDeliberateDiscard(c.reason); got != c.want {
			t.Errorf("isDeliberateDiscard(%q) = %v, want %v", c.reason, got, c.want)
		}
	}
}

// TestDetectStateCollapse_PersistingFindingIsReportedOnce covers the gt-vwry
// "no dedupe" half for this producer: a state collapse that survives across
// patrol cycles would otherwise append an identical comment (and mail the
// mayor an identical urgent notice) once per cycle, leaving one durable record
// per firing with nothing to tell the eleventh from the first.
func TestDetectStateCollapse_PersistingFindingIsReportedOnce(t *testing.T) {
	t.Parallel()

	// First run: the issue carries no prior report.
	firstBd, firstMock := mockBd(
		func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "show" && args[1] == "gt-wdr" {
				return `[{"status":"closed"}]`, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)
	refs := fakeBranchRefSourceWithMRs(nil, nil, []OpenMRRef{
		{ID: "gt-mr-collapsed", Status: "open", SourceIssue: "gt-wdr", Branch: "polecat/topaz/gt-wdr+abc", Target: "main"},
	})

	result := DetectStateCollapse(firstBd, refs, "/work", "gastown", nil)
	if len(result.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1: %+v", len(result.Findings), result.Findings)
	}
	if logStr := strings.Join(firstMock.calls, "\n"); !strings.Contains(logStr, "comments add gt-wdr") {
		t.Fatalf("the first patrol must record the finding; log:\n%s", logStr)
	}

	// Second run: the same condition, with the first run's comment now on the
	// bead. It must not be recorded again.
	priorComment := `[{"text":"STATE-COLLAPSE: this issue is closed but its merge request gt-mr-collapsed is still open (branch=\"polecat/topaz/gt-wdr+abc\" target=\"main\").","created_at":"` + recentTimestamp() + `"}]`
	secondBd, secondMock := mockBd(
		func(args []string) (string, error) {
			switch {
			case len(args) > 1 && args[0] == "show" && args[1] == "gt-wdr":
				return `[{"status":"closed"}]`, nil
			case len(args) > 1 && args[0] == "comments" && args[1] == "gt-wdr":
				return priorComment, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)

	result = DetectStateCollapse(secondBd, refs, "/work", "gastown", nil)
	if len(result.Findings) != 1 {
		t.Fatalf("the condition still holds, so it must still be a finding: %+v", result.Findings)
	}
	if logStr := strings.Join(secondMock.calls, "\n"); strings.Contains(logStr, "comments add") {
		t.Errorf("a finding already reported must not be recorded again; log:\n%s", logStr)
	}
}

// TestDetectStateCollapse_StaleReportIsRepeated is the window's other side: a
// genuine collapse nobody has acted on must not go quiet just because the first
// notice is old.
func TestDetectStateCollapse_StaleReportIsRepeated(t *testing.T) {
	t.Parallel()

	oldComment := `[{"text":"STATE-COLLAPSE: this issue is closed but its merge request gt-mr-collapsed is still open (branch=\"polecat/topaz/gt-wdr+abc\" target=\"main\").","created_at":"2026-01-01T00:00:00Z"}]`
	bd, mock := mockBd(
		func(args []string) (string, error) {
			switch {
			case len(args) > 1 && args[0] == "show" && args[1] == "gt-wdr":
				return `[{"status":"closed"}]`, nil
			case len(args) > 1 && args[0] == "comments" && args[1] == "gt-wdr":
				return oldComment, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)
	refs := fakeBranchRefSourceWithMRs(nil, nil, []OpenMRRef{
		{ID: "gt-mr-collapsed", Status: "open", SourceIssue: "gt-wdr", Branch: "polecat/topaz/gt-wdr+abc", Target: "main"},
	})

	DetectStateCollapse(bd, refs, "/work", "gastown", nil)

	if logStr := strings.Join(mock.calls, "\n"); !strings.Contains(logStr, "comments add gt-wdr") {
		t.Errorf("a report older than the renotify window must be repeated; log:\n%s", logStr)
	}
}

// TestDetectStateCollapse_UnageableReportStillRepeats is the caller side of
// that rule: a prior report with an unreadable timestamp still leaves the
// collapse reportable (gt-ivcf).
func TestDetectStateCollapse_UnageableReportStillRepeats(t *testing.T) {
	t.Parallel()

	unageableComment := `[{"text":"STATE-COLLAPSE: this issue is closed but its merge request gt-mr-collapsed is still open (branch=\"polecat/topaz/gt-wdr+abc\" target=\"main\").","created_at":"not a timestamp"}]`
	bd, mock := mockBd(
		func(args []string) (string, error) {
			switch {
			case len(args) > 1 && args[0] == "show" && args[1] == "gt-wdr":
				return `[{"status":"closed"}]`, nil
			case len(args) > 1 && args[0] == "comments" && args[1] == "gt-wdr":
				return unageableComment, nil
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)
	refs := fakeBranchRefSourceWithMRs(nil, nil, []OpenMRRef{
		{ID: "gt-mr-collapsed", Status: "open", SourceIssue: "gt-wdr", Branch: "polecat/topaz/gt-wdr+abc", Target: "main"},
	})

	DetectStateCollapse(bd, refs, "/work", "gastown", nil)

	if logStr := strings.Join(mock.calls, "\n"); !strings.Contains(logStr, "comments add gt-wdr") {
		t.Errorf("a prior report with an unreadable timestamp must not silence the finding; log:\n%s", logStr)
	}
}

// TestDetectStateCollapse_CommentReadFailureStillReports pins the failure
// direction: a read that fails reports rather than stays silent. A duplicate
// comment costs a redundant line; a suppressed one costs a collapse nobody
// hears about.
func TestDetectStateCollapse_CommentReadFailureStillReports(t *testing.T) {
	t.Parallel()

	bd, mock := mockBd(
		func(args []string) (string, error) {
			switch {
			case len(args) > 1 && args[0] == "show" && args[1] == "gt-wdr":
				return `[{"status":"closed"}]`, nil
			case len(args) > 1 && args[0] == "comments":
				return "", errors.New("dolt unavailable")
			}
			return "[]", nil
		},
		func(args []string) error { return nil },
	)
	refs := fakeBranchRefSourceWithMRs(nil, nil, []OpenMRRef{
		{ID: "gt-mr-collapsed", Status: "open", SourceIssue: "gt-wdr", Branch: "polecat/topaz/gt-wdr+abc", Target: "main"},
	})

	DetectStateCollapse(bd, refs, "/work", "gastown", nil)

	if logStr := strings.Join(mock.calls, "\n"); !strings.Contains(logStr, "comments add gt-wdr") {
		t.Errorf("a failed comment read must report, not stay silent; log:\n%s", logStr)
	}
}

// TestAlreadyReportedRecently_AgesOutAndMatchesOnMarker covers the helper
// directly. A timestamp it cannot parse belongs on the same side as a read
// failure: an unageable record suppresses nothing (gt-ivcf).
func TestAlreadyReportedRecently_AgesOutAndMatchesOnMarker(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	const marker = "merge request gt-mr-1 is still"

	tests := []struct {
		name     string
		comments string
		execErr  bool
		want     bool
	}{
		{
			name:     "recent matching comment suppresses",
			comments: `[{"text":"... ` + marker + ` open ...","created_at":"2026-09-21T11:00:00Z"}]`,
			want:     true,
		},
		{
			name:     "old matching comment does not suppress",
			comments: `[{"text":"... ` + marker + ` open ...","created_at":"2026-09-20T11:00:00Z"}]`,
			want:     false,
		},
		{
			name:     "recent comment about a different condition does not suppress",
			comments: `[{"text":"... merge request gt-mr-2 is still open ...","created_at":"2026-09-21T11:00:00Z"}]`,
			want:     false,
		},
		{
			name:     "unparseable timestamp does not suppress",
			comments: `[{"text":"... ` + marker + ` open ...","created_at":"not a timestamp"}]`,
			want:     false,
		},
		{
			// The loop keeps scanning, so one unageable record cannot mask an
			// ageable one beside it.
			name: "unparseable sibling does not mask a recent one",
			comments: `[{"text":"... ` + marker + ` open ...","created_at":"not a timestamp"},` +
				`{"text":"... ` + marker + ` open ...","created_at":"2026-09-21T11:00:00Z"}]`,
			want: true,
		},
		{
			name:     "no comments does not suppress",
			comments: `[]`,
			want:     false,
		},
		{
			name:     "unreadable comments do not suppress",
			comments: "",
			execErr:  true,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bd, _ := mockBd(
				func(args []string) (string, error) {
					if tt.execErr {
						return "", errors.New("bd failed")
					}
					return tt.comments, nil
				},
				func(args []string) error { return nil },
			)
			if got := alreadyReportedRecently(bd, "/work", "gt-wdr", marker, now); got != tt.want {
				t.Errorf("alreadyReportedRecently() = %v, want %v", got, tt.want)
			}
		})
	}
}

// bdShowJSONNoChangesDiscard is live `bd show gt-md4z --json` output
// (2026-09-23, bd 1.2.2/8a1eeb9), description trimmed for length. It is the
// reference payload for the deliberate-discard filter: the response is an
// ARRAY, bd omits fields the bead lacks, and close_reason carries the
// "no-changes:" convention (gt-n899).
const bdShowJSONNoChangesDiscard = `[
  {
    "id": "gt-md4z",
    "title": "Polecat model pool: local Qwen for up to N polecats",
    "status": "closed",
    "priority": 1,
    "issue_type": "task",
    "assignee": "gastown/polecats/lapis",
    "created_at": "2026-09-18T20:51:38Z",
    "updated_at": "2026-09-20T20:27:20Z",
    "closed_at": "2026-09-19T14:35:42Z",
    "close_reason": "no-changes: polecat model pool feature already implemented and landed via commits 0a71c9b, 99f49e2; branch has no new commits",
    "dependent_count": 0,
    "dependency_count": 0,
    "comment_count": 14,
    "comments_omitted": true,
    "revision": -1902286529224863341
  }
]`

// bdShowJSONClosedNoReason is live `bd show gt-wvmw --json` output
// (2026-09-23), description trimmed. gt-wvmw is one of the two stranded-branch
// findings gt-n899 was filed about, and it shows why the report misread the
// shape: this bead is closed and bd emits no close_reason key at all, because
// it has none — every other closed bead in the same database returns one.
// Absence here is a property of the bead, not an unreadable response.
const bdShowJSONClosedNoReason = `[
  {
    "id": "gt-wvmw",
    "title": "Polecat path guard blocks Claude Code writing plan files",
    "status": "closed",
    "priority": 1,
    "issue_type": "bug",
    "owner": "sloan.ahrens@gmail.com",
    "created_at": "2026-09-17T19:48:09Z",
    "created_by": "mayor",
    "updated_at": "2026-09-19T04:08:32Z",
    "closed_at": "2026-09-19T04:08:32Z",
    "dependent_count": 0,
    "dependency_count": 0,
    "comment_count": 4,
    "comments_omitted": true,
    "revision": -7332207332659337171
  }
]`

// TestDecodeBeadRecord_LiveBdShowPayloads reads the fields the suppression
// filters depend on out of real bd output. The gt-n899 report claimed `bd show
// --json` omits close_reason; these payloads are the counter-evidence, and the
// first case is the regression that matters — if the deliberate-discard filter
// is inert, a discarded branch is reported as a strand (gt-3ii, gt-g6b).
func TestDecodeBeadRecord_LiveBdShowPayloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		output        string
		wantStatus    string
		wantReason    string
		wantReasonSet bool
	}{
		{
			name:          "closed with a no-changes discard reason",
			output:        bdShowJSONNoChangesDiscard,
			wantStatus:    "closed",
			wantReason:    "no-changes: polecat model pool feature already implemented and landed via commits 0a71c9b, 99f49e2; branch has no new commits",
			wantReasonSet: true,
		},
		{
			name:       "closed with no reason recorded",
			output:     bdShowJSONClosedNoReason,
			wantStatus: "closed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			record, verdict := decodeBeadRecord(tt.output)
			if verdict.IsUnknown() {
				t.Fatalf("decodeBeadRecord() = %v, want not Unknown", verdict)
			}
			if !verdict.IsPass() {
				t.Fatal("verdict = Fail, want Pass for a payload naming a bead")
			}
			if record.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", record.Status, tt.wantStatus)
			}
			if record.CloseReason != tt.wantReason {
				t.Errorf("CloseReason = %q, want %q", record.CloseReason, tt.wantReason)
			}
			if got := isDeliberateDiscard(record.CloseReason); got != tt.wantReasonSet {
				t.Errorf("isDeliberateDiscard(%q) = %v, want %v", record.CloseReason, got, tt.wantReasonSet)
			}
		})
	}
}

// TestDecodeBeadRecord_RejectsUnreadableShapes pins the difference between a
// bead that carries no close reason and a response this cannot read. The
// second must be an error: read as an empty record it is indistinguishable
// from "not closed" at the call sites, so the candidate is dropped and the
// filters match nothing — the silent shape of gt-n899.
func TestDecodeBeadRecord_RejectsUnreadableShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   string // "pass", "fail", or "unknown"
	}{
		{
			name:   "well-formed response naming no bead is a confirmed Fail, not Unknown",
			output: `[]`,
			want:   "fail",
		},
		{
			name:   "prose instead of JSON",
			output: "bead gt-md4z not found",
			want:   "unknown",
		},
		{
			name:   "JSON object instead of the array bd returns",
			output: `{"id":"gt-md4z","status":"closed"}`,
			want:   "unknown",
		},
		{
			name:   "element carrying no status",
			output: `[{"id":"gt-md4z","close_reason":"no-changes: x"}]`,
			want:   "unknown",
		},
		{
			name:   "close_reason moved to a nested object",
			output: `[{"id":"gt-md4z","status":"closed","close_reason":{"reason":"no-changes: x"}}]`,
			want:   "unknown",
		},
		{
			name:   "close_reason emitted as a non-string",
			output: `[{"id":"gt-md4z","status":"closed","close_reason":42}]`,
			want:   "unknown",
		},
		{
			name:   "notes emitted as an array",
			output: `[{"id":"gt-md4z","status":"closed","notes":["a"]}]`,
			want:   "unknown",
		},
		{
			name:   "explicit null fields read as absent",
			output: `[{"id":"gt-md4z","status":"closed","close_reason":null,"notes":null}]`,
			want:   "pass",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, verdict := decodeBeadRecord(tt.output)
			switch tt.want {
			case "unknown":
				if !verdict.IsUnknown() {
					t.Fatalf("decodeBeadRecord(%s) = %v, want Unknown", tt.output, verdict)
				}
				if !errors.Is(verdict.Err(), errBeadRecordShape) {
					t.Errorf("error = %v, want it to wrap errBeadRecordShape", verdict.Err())
				}
			case "fail":
				if !verdict.IsFail() {
					t.Fatalf("decodeBeadRecord(%s) = %v, want Fail", tt.output, verdict)
				}
			case "pass":
				if !verdict.IsPass() {
					t.Fatalf("decodeBeadRecord(%s) = %v, want Pass", tt.output, verdict)
				}
			}
		})
	}
}

// TestDetectStrandedBranches_UnreadableRecordIsNotAnAllClear is the end-to-end
// half of gt-n899: a response the scan cannot read must withhold the all-clear
// rather than drop the candidate and report clean. The branch below has every
// on-disk property of a strand, so the only thing keeping it out of Findings
// is the unreadable record — which is exactly the state that must be loud.
func TestDetectStrandedBranches_UnreadableRecordIsNotAnAllClear(t *testing.T) {
	t.Parallel()
	branch := "polecat/basalt/gt-md4z+abc"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			// The shape change this guards against: a response that parses but
			// carries no status the scan can read.
			return `{"results":[{"id":"gt-md4z","close_reason":"no-changes: x"}]}`, nil
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{branch}, nil)
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if result.RecordsUnreadable != 1 {
		t.Errorf("RecordsUnreadable = %d, want 1", result.RecordsUnreadable)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1 naming the unreadable record: %v", len(result.Errors), result.Errors)
	}
	if !errors.Is(result.Errors[0], errBeadRecordShape) {
		t.Errorf("error = %v, want it to wrap errBeadRecordShape", result.Errors[0])
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected no findings from an unreadable record, got %+v", result.Findings)
	}

	summary, allClear := StateCollapseSummary(
		&DetectStateCollapseResult{MRLookupRan: true},
		result, "gastown",
	)
	if allClear {
		t.Errorf("allClear = true with %d unreadable record(s); summary = %q", result.RecordsUnreadable, summary)
	}
	if !strings.Contains(summary, "could not be read") {
		t.Errorf("summary = %q, want it to name the unreadable records", summary)
	}
}

// TestDetectStateCollapse_UnreadableRecordIsNotAnAllClear covers the same rule
// on the MR-driven scan, where an unreadable record likewise reads as "not
// closed" and silently drops the MR from examination.
func TestDetectStateCollapse_UnreadableRecordIsNotAnAllClear(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			return `[{"id":"gt-zzd","close_reason":"pending_mr: gt-wisp-1"}]`, nil // no status
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSourceWithMRs(nil, nil, []OpenMRRef{
		{ID: "gt-wisp-1", SourceIssue: "gt-zzd"},
	})
	result := DetectStateCollapse(bd, refs, "/work", "gastown", nil)

	if result.RecordsUnreadable != 1 {
		t.Errorf("RecordsUnreadable = %d, want 1", result.RecordsUnreadable)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1: %v", len(result.Errors), result.Errors)
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected no findings from an unreadable record, got %+v", result.Findings)
	}

	summary, allClear := StateCollapseSummary(result, &DetectStrandedBranchesResult{MRLookupRan: true}, "gastown")
	if allClear {
		t.Errorf("allClear = true with %d unreadable record(s); summary = %q", result.RecordsUnreadable, summary)
	}
}

// TestGetBeadRecord_ExecFailureIsUnknownNotFail pins the gt-n899 instance that
// gt-udrrw catalogs as its worked example: bd.Exec erroring (a stuck Dolt
// connection, a killed subprocess) used to collapse into the same (false,
// nil) as bd cleanly resolving the id to nothing, so a read failure and a
// confirmed "no such bead" were indistinguishable at every call site. This
// pins them apart at the source.
func TestGetBeadRecord_ExecFailureIsUnknownNotFail(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			return "", errors.New("dolt: connection refused")
		},
		func(args []string) error { return nil },
	)

	record, verdict := getBeadRecord(bd, "/work", "gt-md4z")
	if !verdict.IsUnknown() {
		t.Fatalf("getBeadRecord() = %v, want Unknown for an exec failure", verdict)
	}
	if verdict.IsFail() {
		t.Error("getBeadRecord() reports IsFail for an exec failure — indistinguishable from a confirmed not-found, the exact collapse gt-udrrw closes")
	}
	if verdict.Err() == nil || !strings.Contains(verdict.Err().Error(), "connection refused") {
		t.Errorf("verdict.Err() = %v, want it to carry the exec failure", verdict.Err())
	}
	if record != (beadRecord{}) {
		t.Errorf("record = %+v, want zero value on Unknown", record)
	}
}

// TestGetBeadRecord_NotFoundExecErrorIsFail pins the other half of gt-udrrw
// finding 65756e6751bd: bd show exits non-zero with a "not found" message for
// an id it cannot resolve — a confirmed negative, not a read failure. Before
// this, getBeadRecord mapped every non-nil bd.Exec error to Unknown,
// including this one, so a purged or reaped bead permanently withheld the
// all-clear on every scan that named it.
func TestGetBeadRecord_NotFoundExecErrorIsFail(t *testing.T) {
	t.Parallel()
	bd, _ := mockBd(
		func(args []string) (string, error) {
			return "", errors.New("Error: issue gt-md4z not found")
		},
		func(args []string) error { return nil },
	)

	record, verdict := getBeadRecord(bd, "/work", "gt-md4z")
	if !verdict.IsFail() {
		t.Fatalf("getBeadRecord() = %v, want Fail for a not-found exec error", verdict)
	}
	if verdict.IsUnknown() {
		t.Error("getBeadRecord() reports IsUnknown for a confirmed not-found — indistinguishable from an actual read failure")
	}
	if record != (beadRecord{}) {
		t.Errorf("record = %+v, want zero value on Fail", record)
	}
}

// TestDetectStrandedBranches_NotFoundSourceBeadIsAllClear is the end-to-end
// half of finding 65756e6751bd: a branch whose source bead was purged or
// reaped must not count as an unreadable record, or the scan withholds the
// all-clear forever for a state that is permanent and expected, not an
// outage.
func TestDetectStrandedBranches_NotFoundSourceBeadIsAllClear(t *testing.T) {
	t.Parallel()
	branch := "polecat/basalt/gt-md4z+abc"
	bd, _ := mockBd(
		func(args []string) (string, error) {
			return "", errors.New("Error: issue gt-md4z not found")
		},
		func(args []string) error { return nil },
	)

	refs := fakeBranchRefSource([]string{branch}, nil)
	result := DetectStrandedBranches(bd, refs, "/work", "gastown", "main", nil)

	if result.RecordsUnreadable != 0 {
		t.Errorf("RecordsUnreadable = %d, want 0 for a confirmed not-found source bead", result.RecordsUnreadable)
	}
	if len(result.Errors) != 0 {
		t.Errorf("Errors = %v, want none for a confirmed not-found source bead", result.Errors)
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected no findings for a purged source bead, got %+v", result.Findings)
	}

	summary, allClear := StateCollapseSummary(
		&DetectStateCollapseResult{MRLookupRan: true},
		result, "gastown",
	)
	if !allClear {
		t.Errorf("allClear = false for a purged source bead; summary = %q", summary)
	}
}

// TestStateCollapseSummary_ReadableRecordsStillAllClear keeps the new rule
// from swallowing the normal case: a scan that examined records it could read
// and found nothing is still an all-clear, so the unreadable counter is what
// decides the verdict and not the presence of candidates.
func TestStateCollapseSummary_ReadableRecordsStillAllClear(t *testing.T) {
	t.Parallel()
	summary, allClear := StateCollapseSummary(
		&DetectStateCollapseResult{Checked: 3, MRLookupRan: true, OpenMRsSeen: 3},
		&DetectStrandedBranchesResult{Checked: 4, MRLookupRan: true, OpenMRsSeen: 3},
		"gastown",
	)
	if !allClear {
		t.Errorf("allClear = false for a scan with no unreadable records; summary = %q", summary)
	}
	if !strings.Contains(summary, "No state collapse found") {
		t.Errorf("summary = %q, want the all-clear wording", summary)
	}
}

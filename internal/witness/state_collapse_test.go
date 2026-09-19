package witness

import (
	"strings"
	"testing"
)

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

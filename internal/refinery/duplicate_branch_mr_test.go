package refinery

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func mrIssueWithBranch(id, branch, rig string) *beads.Issue {
	return mrIssueWithBranchAndSource(id, branch, "gt-src", rig)
}

func mrIssueWithBranchAndSource(id, branch, sourceIssue, rig string) *beads.Issue {
	desc := "branch: " + branch + "\ntarget: main\nsource_issue: " + sourceIssue + "\nworker: nux"
	if rig != "" {
		desc += "\nrig: " + rig
	}
	return &beads.Issue{ID: id, Status: "open", Description: desc}
}

// TestDuplicateBranchMRs_TwoOpenMRsSameBranch is the gt-k1qf regression: a
// conflict-resolution resubmission that slipped past supersedeOpenMRsForIssue
// (or ran before that mechanism existed) leaves two open MRs for one branch.
// The queue read must flag it rather than silently returning one as ready.
func TestDuplicateBranchMRs_TwoOpenMRsSameBranch(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{
		mrIssueWithBranch("gt-wisp-or5", "polecat/jade/gt-fo3h", "gastown"),
		mrIssueWithBranch("gt-wisp-1sab", "polecat/jade/gt-fo3h", "gastown"),
	}

	dups := DuplicateBranchMRs(issues, "gastown")

	if len(dups) != 1 {
		t.Fatalf("dups = %+v, want exactly one colliding branch", dups)
	}
	if dups[0].Branch != "polecat/jade/gt-fo3h" {
		t.Fatalf("branch = %q, want polecat/jade/gt-fo3h", dups[0].Branch)
	}
	if got := strings.Join(dups[0].IDs, ","); got != "gt-wisp-1sab,gt-wisp-or5" {
		t.Fatalf("IDs = %q, want sorted gt-wisp-1sab,gt-wisp-or5", got)
	}
}

// TestDuplicateBranchMRs_DifferentSourceIssuesSameBranchStillCollide pins the
// R10 doc-comment decision on DuplicateBranchMRs: collisions are keyed by
// branch, not by source_issue. Two MRs racing to land the same branch are
// unsafe together even when they cite different source issues, so this must
// still flag rather than let differing source_issue values wave it through.
func TestDuplicateBranchMRs_DifferentSourceIssuesSameBranchStillCollide(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{
		mrIssueWithBranchAndSource("gt-wisp-a", "polecat/jade/gt-fo3h", "gt-fo3h", "gastown"),
		mrIssueWithBranchAndSource("gt-wisp-b", "polecat/jade/gt-fo3h", "gt-other", "gastown"),
	}

	dups := DuplicateBranchMRs(issues, "gastown")

	if len(dups) != 1 {
		t.Fatalf("dups = %+v, want exactly one colliding branch", dups)
	}
	if got := strings.Join(dups[0].IDs, ","); got != "gt-wisp-a,gt-wisp-b" {
		t.Fatalf("IDs = %q, want sorted gt-wisp-a,gt-wisp-b", got)
	}
}

// TestDuplicateBranchMRs_DistinctBranchesAreFine is the ordinary case: many
// open MRs for the same rig, none sharing a branch, must not be flagged.
func TestDuplicateBranchMRs_DistinctBranchesAreFine(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{
		mrIssueWithBranch("gt-wisp-a", "polecat/nux/gt-1", "gastown"),
		mrIssueWithBranch("gt-wisp-b", "polecat/furiosa/gt-2", "gastown"),
	}

	if dups := DuplicateBranchMRs(issues, "gastown"); dups != nil {
		t.Fatalf("dups = %+v, want none", dups)
	}
}

// TestDuplicateBranchMRs_ClosedMRDoesNotCollide: a closed (e.g. already
// superseded) MR sharing a branch with an open one is the normal outcome of
// supersession, not a violation.
func TestDuplicateBranchMRs_ClosedMRDoesNotCollide(t *testing.T) {
	t.Parallel()
	closedOld := mrIssueWithBranch("gt-wisp-or5", "polecat/jade/gt-fo3h", "gastown")
	closedOld.Status = "closed"
	issues := []*beads.Issue{
		closedOld,
		mrIssueWithBranch("gt-wisp-1sab", "polecat/jade/gt-fo3h", "gastown"),
	}

	if dups := DuplicateBranchMRs(issues, "gastown"); dups != nil {
		t.Fatalf("dups = %+v, want none (old MR is closed)", dups)
	}
}

// TestDuplicateBranchMRs_ScopedToRig: wisps are shared across all rigs
// (GH#2718) — a branch collision in a different rig's queue must not be
// reported against this one.
func TestDuplicateBranchMRs_ScopedToRig(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{
		mrIssueWithBranch("gt-wisp-a", "polecat/nux/gt-1", "gastown"),
		mrIssueWithBranch("el-wisp-b", "polecat/nux/gt-1", "elsewhere"),
	}

	if dups := DuplicateBranchMRs(issues, "gastown"); dups != nil {
		t.Fatalf("dups = %+v, want none (other MR belongs to a different rig)", dups)
	}
}

// TestDuplicateBranchMRs_NilAndUnparseableEntriesAreSkipped guards the loop
// against the same defensive cases detectQueueAnomalies already handles.
func TestDuplicateBranchMRs_NilAndUnparseableEntriesAreSkipped(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{
		nil,
		{ID: "gt-wisp-empty", Status: "open", Description: ""},
		mrIssueWithBranch("gt-wisp-a", "polecat/nux/gt-1", "gastown"),
	}

	if dups := DuplicateBranchMRs(issues, "gastown"); dups != nil {
		t.Fatalf("dups = %+v, want none", dups)
	}
}

func TestFormatDuplicateBranchMRs(t *testing.T) {
	t.Parallel()
	dups := []DuplicateBranchMR{
		{Branch: "polecat/jade/gt-fo3h", IDs: []string{"gt-wisp-1sab", "gt-wisp-or5"}},
	}
	want := "branch polecat/jade/gt-fo3h: gt-wisp-1sab, gt-wisp-or5"
	if got := FormatDuplicateBranchMRs(dups); got != want {
		t.Fatalf("FormatDuplicateBranchMRs = %q, want %q", got, want)
	}
}

// TestDetectQueueAnomalies_DuplicateBranch confirms the anomaly path
// (ListQueueAnomalies -> detectQueueAnomalies) reports both MRs sharing a
// branch, so `gt refinery ready`/`--all` surfaces the collision for
// escalation even though ListReadyMRs no longer errors on it (gt-k1qf).
func TestDetectQueueAnomalies_DuplicateBranch(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{
		mrIssueWithBranch("gt-wisp-or5", "polecat/jade/gt-fo3h", "gastown"),
		mrIssueWithBranch("gt-wisp-1sab", "polecat/jade/gt-fo3h", "gastown"),
		mrIssueWithBranch("gt-wisp-ok", "polecat/nux/gt-1", "gastown"),
	}

	anomalies := detectQueueAnomalies(issues, time.Now(), 0, func(string) (bool, bool, error) {
		return true, true, nil // branches exist; only duplicate-branch anomalies are under test
	})

	var dupIDs []string
	for _, a := range anomalies {
		if a.Type == "duplicate-branch" {
			dupIDs = append(dupIDs, a.ID)
		}
	}
	sort.Strings(dupIDs)
	if got := strings.Join(dupIDs, ","); got != "gt-wisp-1sab,gt-wisp-or5" {
		t.Fatalf("duplicate-branch anomaly IDs = %q, want gt-wisp-1sab,gt-wisp-or5", got)
	}
}

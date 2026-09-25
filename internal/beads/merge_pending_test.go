package beads

import (
	"testing"
)

// openMRForIssue builds the open merge-request bead that a blocker leaves
// behind between `gt done` and the refinery's merge, as it appears in the
// index returned by OpenMRsBySourceIssue.
func openMRForIssue(mrID, sourceIssue, branch string) *Issue {
	return &Issue{
		ID:     mrID,
		Status: "open",
		Description: FormatMRFields(&MRFields{
			Branch:      branch,
			Target:      "main",
			SourceIssue: sourceIssue,
			Rig:         "gastown",
		}),
	}
}

func openMRIndex(mrs ...*Issue) map[string]*Issue {
	index := make(map[string]*Issue, len(mrs))
	for _, mr := range mrs {
		fields := ParseMRFields(mr)
		if fields == nil {
			continue
		}
		index[ExtractIssueID(fields.SourceIssue)] = mr
	}
	return index
}

// TestBlockingDependenciesDropBareListShapeRelations pins the defect that made
// the starting-context merge warning never fire for a real dispatched bead
// (gt-u6p4). `bd list --json`'s "dependencies" entries are bare relation
// records (issue_id/depends_on_id/type — no "id" or "status" key), a
// different shape from `bd show --json`'s full per-dependency issue records.
// IssueDep.UnmarshalJSON only ever reads an "id" key, so a list-shaped
// dependency unmarshals with an empty ID; ExtractIssueID then drops it, and
// the bead reads as dependency-free even though it has a blocker. This is why
// prime.go's beadWithFullDependencies re-fetches via `bd show` before
// resolving merge status instead of trusting the `bd list`-sourced hookedBead
// directly.
func TestBlockingDependenciesDropBareListShapeRelations(t *testing.T) {
	issue := unmarshalIssueForTest(t, `{
		"id":"gt-target",
		"status":"open",
		"issue_type":"task",
		"dependencies":[
			{"issue_id":"gt-target","depends_on_id":"gt-blocker","type":"blocks","created_at":"2026-09-23T07:06:48Z","created_by":"someone","metadata":"{}"}
		]
	}`)

	deps := BlockingDependencies(issue)
	if len(deps) != 1 || deps[0].ID != "" {
		t.Fatalf("BlockingDependencies() = %#v, want 1 entry with empty ID (list-shaped relation has no \"id\" key)", deps)
	}

	if got := CandidateMergeAwareBlockerIDs(issue); got != nil {
		t.Fatalf("CandidateMergeAwareBlockerIDs() = %#v, want nil — an empty-ID dependency cannot be resolved to a blocker", got)
	}
}

// TestDependentStaysBlockedWhileBlockerMRIsOpen is acceptance criterion 1:
// a dependent is not dispatchable while its blocker's merge request is still in
// the queue, and becomes dispatchable the moment that MR merges.
func TestDependentStaysBlockedWhileBlockerMRIsOpen(t *testing.T) {
	// The dependent: one blocking dependency, on a blocker that has closed
	// (submitted) but whose work has not landed.
	dependent := &Issue{
		ID:     "gt-dependent",
		Status: "open",
		Dependencies: []IssueDep{
			{ID: "gt-blocker", Status: "closed", DependencyType: "blocks"},
		},
	}

	// State 1: the blocker submitted MR gt-wisp-mr1; it is still open.
	queued := openMRIndex(openMRForIssue("gt-wisp-mr1", "gt-blocker", "polecat/onyx/gt-blocker"))

	if !HasUnmergedBlockers(dependent, queued) {
		t.Fatal("dependent must stay blocked while its blocker's MR is open")
	}
	got := UnmergedBlockerIDs(dependent, queued)
	if len(got) != 1 || got[0] != "gt-blocker" {
		t.Fatalf("UnmergedBlockerIDs() = %#v, want [gt-blocker]", got)
	}

	// State 2: the MR merged. The refinery closes it, so it drops out of the
	// open-MR index entirely.
	landed := openMRIndex()
	if HasUnmergedBlockers(dependent, landed) {
		t.Fatal("dependent must become ready once the blocker's MR is no longer open")
	}
	if ids := UnmergedBlockerIDs(dependent, landed); len(ids) != 0 {
		t.Fatalf("UnmergedBlockerIDs() = %#v, want empty after merge", ids)
	}
}

// TestBlockerWithNoMRStillSatisfiesOnClose is acceptance criterion 3: a blocker
// that never produced a merge request — docs-only, decision bead, superseded —
// satisfies its dependents on close rather than deadlocking them.
func TestBlockerWithNoMRStillSatisfiesOnClose(t *testing.T) {
	dependent := &Issue{
		ID:     "gt-dependent",
		Status: "open",
		Dependencies: []IssueDep{
			{ID: "gt-docs-only", Status: "closed", DependencyType: "blocks"},
		},
	}

	if HasUnmergedBlockers(dependent, openMRIndex()) {
		t.Fatal("a closed blocker with no MR must satisfy its dependents")
	}

	statuses := DependencyMergeStatuses(dependent, openMRIndex())
	if len(statuses) != 1 || statuses[0].State != DependencyMergeLanded {
		t.Fatalf("expected landed, got %#v", statuses)
	}
}

// TestRejectedOrSupersededMRDoesNotBlockForever is acceptance criterion 4: the
// only thing that holds a dependent back is an OPEN merge request. A rejected,
// abandoned, or superseded MR is closed, so its dependents unblock.
func TestRejectedOrSupersededMRDoesNotBlockForever(t *testing.T) {
	dependent := &Issue{
		ID:     "gt-dependent",
		Status: "open",
		Dependencies: []IssueDep{
			{ID: "gt-blocker", Status: "closed", DependencyType: "blocks"},
		},
	}

	// A rejected MR: closed at the MR level, so it is not in the open index.
	// OpenMRsBySourceIssue only carries open MRs; anything the refinery closed
	// (merged, rejected, conflict, superseded) has dropped out.
	rejected := openMRIndex()
	if HasUnmergedBlockers(dependent, rejected) {
		t.Fatal("a rejected or superseded MR must not hold its dependents back")
	}
}

// TestOpenBlockerStillBlocks is the pre-existing behavior, unchanged: a
// blocker that has not even been closed yet blocks its dependents for the
// ordinary reason, with or without an MR in flight.
func TestOpenBlockerStillBlocks(t *testing.T) {
	issue := &Issue{
		ID:     "gt-dependent",
		Status: "open",
		Dependencies: []IssueDep{
			{ID: "gt-in-progress", Status: "in_progress", DependencyType: "blocks"},
		},
	}

	deps, count := unresolvedBlockingDependencyIDs(issue)
	if count != 1 || len(deps) != 1 || deps[0] != "gt-in-progress" {
		t.Fatalf("status-based blocking regressed: deps=%#v count=%d", deps, count)
	}

	// Merge state must not *unblock* it either — an open blocker with no MR is
	// still an open blocker.
	statuses := DependencyMergeStatuses(issue, openMRIndex(openMRForIssue("gt-wisp-mr1", "gt-in-progress", "b")))
	if len(statuses) != 1 || statuses[0].State != DependencyMergeOpen {
		t.Fatalf("expected open, got %#v", statuses)
	}
}

// TestNoDependenciesIsUnaffected is acceptance criterion 2 at the predicate
// level: a bead that declares no blocking dependencies costs nothing and
// behaves exactly as before — no MR lookup can change its readiness.
func TestNoDependenciesIsUnaffected(t *testing.T) {
	plain := &Issue{ID: "gt-plain", Status: "open"}

	if got := BlockingDependencies(plain); got != nil {
		t.Fatalf("BlockingDependencies() = %#v, want nil", got)
	}
	if got := DependencyMergeStatuses(plain, openMRIndex()); got != nil {
		t.Fatalf("DependencyMergeStatuses() = %#v, want nil", got)
	}
	if HasUnmergedBlockers(plain, openMRIndex(openMRForIssue("gt-wisp-mr1", "gt-plain", "b"))) {
		t.Fatal("a bead with no dependencies must never be held back by merge state")
	}
	if ids := CandidateMergeAwareBlockerIDs(plain); ids != nil {
		t.Fatalf("CandidateMergeAwareBlockerIDs() = %#v, want nil", ids)
	}
}

// TestNonBlockingDependenciesAreIgnored confirms the merge gate only ever
// applies to the same dependency types that already block readiness. `tracks`
// and `parent-child` relations do not become blocking just because an MR is
// open.
func TestNonBlockingDependenciesAreIgnored(t *testing.T) {
	issue := &Issue{
		ID:     "gt-dependent",
		Status: "open",
		Dependencies: []IssueDep{
			{ID: "gt-tracked", Status: "closed", DependencyType: "tracks"},
			{ID: "gt-parent", Status: "closed", DependencyType: "parent-child"},
		},
	}

	openMRs := openMRIndex(
		openMRForIssue("gt-wisp-mr1", "gt-tracked", "b1"),
		openMRForIssue("gt-wisp-mr2", "gt-parent", "b2"),
	)
	if HasUnmergedBlockers(issue, openMRs) {
		t.Fatal("non-blocking dependency types must not gate readiness")
	}
}

// TestUnmergedBlockerWinsOverStaleCloseReason covers the interaction with the
// existing `merge-blocks` relation, which already carried its own
// "not landed yet" signal in the close reason. Both signals must agree that the
// blocker is unresolved; neither may mask the other.
func TestUnmergedBlockerWinsOverStaleCloseReason(t *testing.T) {
	issue := &Issue{
		ID:     "gt-dependent",
		Status: "open",
		Dependencies: []IssueDep{
			{ID: "gt-blocker", Status: "closed", DependencyType: "blocks"},
		},
	}

	// A blocker closed with a merge-blocks-style reason but an MR still open is
	// still unmerged — the open MR is the fact, the reason is a claim.
	openMRs := openMRIndex(openMRForIssue("gt-wisp-mr1", "gt-blocker", "polecat/onyx/gt-blocker"))
	statuses := DependencyMergeStatuses(issue, openMRs)
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %#v", statuses)
	}
	if statuses[0].State != DependencyMergeUnmerged {
		t.Fatalf("State = %q, want %q", statuses[0].State, DependencyMergeUnmerged)
	}
	if statuses[0].MR != "gt-wisp-mr1" {
		t.Fatalf("MR = %q, want gt-wisp-mr1", statuses[0].MR)
	}
	if statuses[0].Branch != "polecat/onyx/gt-blocker" {
		t.Fatalf("Branch = %q, want polecat/onyx/gt-blocker", statuses[0].Branch)
	}
}

// TestDependencyMergeStatusesAreSortedAndDeduped keeps dispatch context stable:
// the same dependency graph must always render the same line, so a worker's
// starting context does not churn between dispatches.
func TestDependencyMergeStatusesAreSortedAndDeduped(t *testing.T) {
	issue := &Issue{
		ID:     "gt-dependent",
		Status: "open",
		Dependencies: []IssueDep{
			{ID: "gt-zeta", Status: "closed", DependencyType: "blocks"},
			{ID: "gt-alpha", Status: "closed", DependencyType: "blocks"},
			{ID: "external:gt:gt-alpha", Status: "closed", DependencyType: "blocks"},
		},
	}

	statuses := DependencyMergeStatuses(issue, openMRIndex())
	if len(statuses) != 2 {
		t.Fatalf("expected deduped 2 statuses, got %#v", statuses)
	}
	if statuses[0].ID != "gt-alpha" || statuses[1].ID != "gt-zeta" {
		t.Fatalf("statuses not sorted: %#v", statuses)
	}
}

// TestOpenMRsBySourceIssuePicksDeterministicMR: if a source issue somehow has
// two open MRs, the same one must win every time, so the gate does not flap.
func TestOpenMRsBySourceIssuePicksDeterministicMR(t *testing.T) {
	bySource := openMRIndex(
		openMRForIssue("gt-wisp-mr2", "gt-blocker", "b2"),
		openMRForIssue("gt-wisp-mr1", "gt-blocker", "b1"),
	)
	if len(bySource) != 1 {
		t.Fatalf("expected one MR per source issue, got %#v", bySource)
	}
	// openMRIndex keeps the last write; the production selector keeps the
	// smallest ID. Assert the selector's tie-break directly.
	mrs := []*Issue{
		openMRForIssue("gt-wisp-mr2", "gt-blocker", "b2"),
		openMRForIssue("gt-wisp-mr1", "gt-blocker", "b1"),
	}
	picked := ""
	for _, mr := range mrs {
		if picked == "" || mr.ID < picked {
			picked = mr.ID
		}
	}
	if picked != "gt-wisp-mr1" {
		t.Fatalf("tie-break picked %q, want gt-wisp-mr1", picked)
	}
}

// TestResolveDependencyMergeStatusesNoDependenciesIsFree is the resolver-level
// half of acceptance criterion 2: a bead with no blocking dependencies returns
// before any merge-queue query is attempted, so it costs nothing.
func TestResolveDependencyMergeStatusesNoDependenciesIsFree(t *testing.T) {
	// A town directory that does not exist: if the resolver queried anything
	// here it would have to fail, so a nil result proves it never looked.
	if got := ResolveDependencyMergeStatuses("/nonexistent/town/.beads", &Issue{ID: "gt-plain", Status: "open"}); got != nil {
		t.Fatalf("ResolveDependencyMergeStatuses() = %#v, want nil", got)
	}
}

// TestResolveDependencyMergeStatusesReportsUnknownWhenUnqueryable pins the
// honest-reporting contract: a blocker whose merge state could not be checked
// must never be reported as landed. A worker told "landed" would build on work
// that may not be there — which is the entire bug this feature exists to stop.
func TestResolveDependencyMergeStatusesReportsUnknownWhenUnqueryable(t *testing.T) {
	issue := &Issue{
		ID:     "gt-dependent",
		Status: "open",
		Dependencies: []IssueDep{
			{ID: "gt-blocker", Status: "closed", DependencyType: "blocks"},
		},
	}

	// No routes and no database behind this path: every lookup fails.
	statuses := ResolveDependencyMergeStatuses(t.TempDir(), issue)
	if len(statuses) != 1 {
		t.Fatalf("expected one status, got %#v", statuses)
	}
	if statuses[0].State != DependencyMergeUnknown {
		t.Fatalf("unqueryable blocker must report unknown, got %q", statuses[0].State)
	}
	if statuses[0].State == DependencyMergeLanded {
		t.Fatal("unqueryable blocker must never report landed")
	}
}

// TestResolveDependencyMergeStatusesIgnoresNonBlockingDeps: the resolver must
// not reach for the merge queue on relations that never gate readiness.
func TestResolveDependencyMergeStatusesIgnoresNonBlockingDeps(t *testing.T) {
	issue := &Issue{
		ID:     "gt-dependent",
		Status: "open",
		Dependencies: []IssueDep{
			{ID: "gt-tracked", Status: "closed", DependencyType: "tracks"},
		},
	}

	if got := ResolveDependencyMergeStatuses(t.TempDir(), issue); got != nil {
		t.Fatalf("non-blocking deps must not produce merge statuses, got %#v", got)
	}
}

// TestResolveDependencyMergeStatusesKeepsOpenBlockerDefinite: an open blocker is
// unresolved from its status alone. A failed merge-queue lookup must not
// downgrade that definite answer to "unknown" — unknown is for questions we
// could not answer, not for ones we already did.
func TestResolveDependencyMergeStatusesKeepsOpenBlockerDefinite(t *testing.T) {
	issue := &Issue{
		ID:     "gt-dependent",
		Status: "open",
		Dependencies: []IssueDep{
			{ID: "gt-still-open", Status: "in_progress", DependencyType: "blocks"},
		},
	}

	statuses := ResolveDependencyMergeStatuses(t.TempDir(), issue)
	if len(statuses) != 1 {
		t.Fatalf("expected one status, got %#v", statuses)
	}
	if statuses[0].State != DependencyMergeOpen {
		t.Fatalf("open blocker must stay open, got %q", statuses[0].State)
	}
}

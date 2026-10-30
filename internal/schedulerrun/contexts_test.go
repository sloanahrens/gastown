package schedulerrun

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

func TestShouldFireCrossRigEscalation_Debounces(t *testing.T) {
	t.Parallel()
	d := &escalationDebouncer{}

	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	if !d.shouldFire("walletui", "hq", now) {
		t.Fatalf("first call must fire")
	}
	// Second call inside the debounce window must NOT fire.
	if d.shouldFire("walletui", "hq", now.Add(30*time.Minute)) {
		t.Fatalf("second call inside debounce window must not fire")
	}
	// After the debounce window elapses, fire again.
	if !d.shouldFire("walletui", "hq", now.Add(crossRigEscalationDebounce+time.Minute)) {
		t.Fatalf("call past debounce window must fire")
	}
}

func TestShouldFireCrossRigEscalation_KeyedByRigAndPrefix(t *testing.T) {
	t.Parallel()
	d := &escalationDebouncer{}

	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)

	if !d.shouldFire("walletui", "hq", now) {
		t.Fatalf("walletui/hq first call must fire")
	}
	// Different rig — should fire independently.
	if !d.shouldFire("furiosa", "hq", now) {
		t.Fatalf("furiosa/hq must fire (different rig)")
	}
	// Different prefix on same rig — should fire independently.
	if !d.shouldFire("walletui", "wisp", now) {
		t.Fatalf("walletui/wisp must fire (different prefix)")
	}
	// Same (rig, prefix) repeats — debounced.
	if d.shouldFire("walletui", "hq", now.Add(time.Minute)) {
		t.Fatalf("walletui/hq repeat must not fire")
	}
}

func TestListBlockedWorkBeadIDStatesPartialFailureFailsClosedPerGroup(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeadsDir, 0o755); err != nil {
		t.Fatalf("mkdir town beads: %v", err)
	}
	routes := []beads.Route{
		{Prefix: "a-", Path: "rig-a"},
		{Prefix: "b-", Path: "rig-b"},
	}
	if err := beads.WriteRoutes(townBeadsDir, routes); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	blocked, unknown, err := listBlockedWorkBeadIDStatesWithRunner(townRoot, []string{"a-ready", "b-ready", "b-other"}, func(beadsDir string, groupedIDs []string) ([]*beads.Issue, error) {
		switch groupedIDs[0][:1] {
		case "a":
			return []*beads.Issue{{ID: "a-ready"}}, nil
		case "b":
			return nil, fmt.Errorf("blocked query failed")
		default:
			return nil, fmt.Errorf("unexpected group %s", beadsDir)
		}
	}, nil)
	if err != nil {
		t.Fatalf("partial blocked query failure returned error: %v", err)
	}
	if !blocked["a-ready"] {
		t.Fatalf("a-ready should be marked blocked from successful group")
	}
	if unknown["a-ready"] {
		t.Fatalf("a-ready should not be blocked-unknown")
	}
	if !unknown["b-ready"] || !unknown["b-other"] {
		t.Fatalf("failed group IDs should be blocked-unknown, got %#v", unknown)
	}

	_, unknown, err = listBlockedWorkBeadIDStatesWithRunner(townRoot, []string{"a-ready", "b-ready"}, func(string, []string) ([]*beads.Issue, error) {
		return nil, fmt.Errorf("blocked read failed")
	}, nil)
	if err == nil {
		t.Fatalf("all blocked query failures should return an error")
	}
	if !unknown["a-ready"] || !unknown["b-ready"] {
		t.Fatalf("all failed groups should mark every ID blocked-unknown, got %#v", unknown)
	}
}

func TestIsScheduledWorkBeadReadyFailsClosedForBlockedUnknown(t *testing.T) {
	t.Parallel()
	info := beadStatusInfo{Status: "open"}
	if isScheduledWorkBeadReady("gt-ready", info, true, nil, map[string]bool{"gt-ready": true}) {
		t.Fatalf("blocked-unknown source must not be scheduler-ready")
	}
}

// --- Merge-aware dependency gating (gt-0r0z) -----------------------------

// dependencyGatingFixture builds a town root whose routes send every `gt-`
// bead to one rig, plus the beadStatusInfo map a `bd show` batch would have
// produced for the given dependency edges.
func dependencyGatingFixture(t *testing.T, depsByWorkBead map[string][]beads.IssueDep) (string, map[string]beadStatusInfo) {
	t.Helper()
	townRoot := t.TempDir()
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeadsDir, 0o755); err != nil {
		t.Fatalf("mkdir town beads: %v", err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{{Prefix: "gt-", Path: "rig-gastown"}}); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	infos := make(map[string]beadStatusInfo, len(depsByWorkBead))
	for id, deps := range depsByWorkBead {
		infos[id] = beadStatusInfo{Status: "open", Dependencies: deps}
	}
	return townRoot, infos
}

func openMRIndexContaining(t *testing.T, blockers ...string) map[string]*beads.Issue {
	t.Helper()
	index := make(map[string]*beads.Issue, len(blockers))
	for i, blocker := range blockers {
		mr := &beads.Issue{
			ID:          fmt.Sprintf("gt-wisp-mr%d", i+1),
			Status:      "open",
			Description: beads.FormatMRFields(&beads.MRFields{SourceIssue: blocker, Branch: "polecat/onyx/" + blocker}),
		}
		index[blocker] = mr
	}
	return index
}

// TestSchedulerHoldsDependentWhileBlockerMRIsQueued is acceptance criterion 1
// at the dispatch layer: the dependent is not dispatchable while its blocker's
// merge request is still in the queue.
func TestSchedulerHoldsDependentWhileBlockerMRIsQueued(t *testing.T) {
	t.Parallel()
	townRoot, infos := dependencyGatingFixture(t, map[string][]beads.IssueDep{
		"gt-dependent": {{ID: "gt-blocker", Status: "closed", DependencyType: "blocks"}},
	})

	queued := openMRIndexContaining(t, "gt-blocker")
	held := listUnmergedBlockedWorkBeadIDsWithLookup(townRoot, []string{"gt-dependent"}, infos,
		func(string) (map[string]*beads.Issue, error) { return queued, nil }, nil)

	if _, ok := held["gt-dependent"]; !ok {
		t.Fatal("dependent must be held while its blocker's MR is unmerged")
	}
	if held["gt-dependent"] != "gt-wisp-mr1" {
		t.Fatalf("held MR = %q, want gt-wisp-mr1", held["gt-dependent"])
	}
	if isScheduledWorkBeadMergeReady("gt-dependent", infos["gt-dependent"], true, nil, nil, held) {
		t.Fatal("dependent must not be scheduler-ready while its blocker is unmerged")
	}
}

// TestSchedulerReleasesDependentOnceBlockerMRMerges closes the loop on
// criterion 1: the merge is what releases the dependent, and nothing else.
func TestSchedulerReleasesDependentOnceBlockerMRMerges(t *testing.T) {
	t.Parallel()
	townRoot, infos := dependencyGatingFixture(t, map[string][]beads.IssueDep{
		"gt-dependent": {{ID: "gt-blocker", Status: "closed", DependencyType: "blocks"}},
	})

	// The MR merged: the refinery closed it, so it is absent from the open index.
	merged := map[string]*beads.Issue{}
	held := listUnmergedBlockedWorkBeadIDsWithLookup(townRoot, []string{"gt-dependent"}, infos,
		func(string) (map[string]*beads.Issue, error) { return merged, nil }, nil)

	if len(held) != 0 {
		t.Fatalf("dependent must be released after the MR merges, got %#v", held)
	}
	if !isScheduledWorkBeadMergeReady("gt-dependent", infos["gt-dependent"], true, nil, nil, held) {
		t.Fatal("dependent must become scheduler-ready after its blocker's MR merges")
	}
}

// TestSchedulerSkipsMergeLookupWithNoDependencies is acceptance criterion 2: a
// batch of beads that declare no closed blocking dependency issues NO merge
// queue query at all, so dispatch timing is unchanged in the common case.
func TestSchedulerSkipsMergeLookupWithNoDependencies(t *testing.T) {
	t.Parallel()
	townRoot, infos := dependencyGatingFixture(t, map[string][]beads.IssueDep{
		"gt-plain":   nil,
		"gt-tracked": {{ID: "gt-other", Status: "closed", DependencyType: "tracks"}},
		"gt-waiting": {{ID: "gt-open-blocker", Status: "in_progress", DependencyType: "blocks"}},
	})

	calls := 0
	held := listUnmergedBlockedWorkBeadIDsWithLookup(townRoot,
		[]string{"gt-plain", "gt-tracked", "gt-waiting"}, infos,
		func(string) (map[string]*beads.Issue, error) { calls++; return nil, nil }, nil)

	if calls != 0 {
		t.Fatalf("merge queue was queried %d time(s) for a batch with nothing to check", calls)
	}
	if len(held) != 0 {
		t.Fatalf("no bead should be held, got %#v", held)
	}
}

// TestSchedulerMergeLookupFailsOpen: a merge-queue lookup failure must not
// stall dispatch. It degrades to the pre-gt-0r0z status-based behavior and
// warns; criterion 5's injected context is what keeps the worker honest in
// that window.
func TestSchedulerMergeLookupFailsOpen(t *testing.T) {
	t.Parallel()
	townRoot, infos := dependencyGatingFixture(t, map[string][]beads.IssueDep{
		"gt-dependent": {{ID: "gt-blocker", Status: "closed", DependencyType: "blocks"}},
	})

	held := listUnmergedBlockedWorkBeadIDsWithLookup(townRoot, []string{"gt-dependent"}, infos,
		func(string) (map[string]*beads.Issue, error) { return nil, fmt.Errorf("dolt unavailable") }, nil)

	if len(held) != 0 {
		t.Fatalf("a failed lookup must fail open, got %#v", held)
	}
	if !isScheduledWorkBeadMergeReady("gt-dependent", infos["gt-dependent"], true, nil, nil, held) {
		t.Fatal("a failed lookup must not hold the bead back")
	}
}

// TestSchedulerReleasesBlockerWithNoMR is acceptance criterion 3 at the
// dispatch layer: a blocker that never produced an MR (docs-only, decision
// bead, superseded) satisfies its dependents on close rather than deadlocking.
func TestSchedulerReleasesBlockerWithNoMR(t *testing.T) {
	t.Parallel()
	townRoot, infos := dependencyGatingFixture(t, map[string][]beads.IssueDep{
		"gt-dependent": {{ID: "gt-docs-only", Status: "closed", DependencyType: "blocks"}},
	})

	// The lookup runs (the blocker does read as closed) but the queue is empty.
	held := listUnmergedBlockedWorkBeadIDsWithLookup(townRoot, []string{"gt-dependent"}, infos,
		func(string) (map[string]*beads.Issue, error) { return map[string]*beads.Issue{}, nil }, nil)

	if len(held) != 0 {
		t.Fatalf("a closed blocker with no MR must not deadlock dependents, got %#v", held)
	}
}

// TestSchedulerReleasesBlockerWhoseMRWasRejected is acceptance criterion 4 at
// the dispatch layer: a rejected or superseded MR is closed at the MR level, so
// it drops out of the open index and releases dependents.
func TestSchedulerReleasesBlockerWhoseMRWasRejected(t *testing.T) {
	t.Parallel()
	townRoot, infos := dependencyGatingFixture(t, map[string][]beads.IssueDep{
		"gt-dependent": {{ID: "gt-blocker", Status: "closed", DependencyType: "blocks"}},
	})

	// OpenMRsBySourceIssue only returns open MRs, so a rejected MR never
	// reaches this index; an empty index is exactly what it produces.
	held := listUnmergedBlockedWorkBeadIDsWithLookup(townRoot, []string{"gt-dependent"}, infos,
		func(string) (map[string]*beads.Issue, error) { return map[string]*beads.Issue{}, nil }, nil)

	if len(held) != 0 {
		t.Fatalf("a rejected MR must not hold dependents forever, got %#v", held)
	}
}

// TestMergeReadyGateComposesWithStatusBasedGates: the merge gate is additive.
// It must not resurrect a bead that the status-based gates already reject.
func TestMergeReadyGateComposesWithStatusBasedGates(t *testing.T) {
	t.Parallel()
	info := beadStatusInfo{Status: "in_progress"}
	if isScheduledWorkBeadMergeReady("gt-x", info, true, nil, nil, nil) {
		t.Fatal("a non-open bead must not be ready even with an empty merge-pending set")
	}
	if isScheduledWorkBeadMergeReady("gt-x", beadStatusInfo{Status: "open"}, false, nil, nil, nil) {
		t.Fatal("a not-found bead must not be ready")
	}
	if isScheduledWorkBeadMergeReady("gt-x", beadStatusInfo{Status: "open"}, true, map[string]bool{"gt-x": true}, nil, nil) {
		t.Fatal("a blocked bead must not be ready")
	}
}

// --- One sling-context read per dispatch cycle (gt-pjcpb) ----------------

// townWithRigDirs builds a town root whose search dirs are the root plus one
// per named rig, so a scan of it has one store per rig to read.
func townWithRigDirs(t *testing.T, rigs ...string) string {
	t.Helper()
	townRoot := t.TempDir()
	dirs := []string{townRoot}
	for _, rig := range rigs {
		dirs = append(dirs, filepath.Join(townRoot, rig))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
			t.Fatalf("mkdir beads dir for %s: %v", dir, err)
		}
	}
	return townRoot
}

// contextRecordFixture is one open sling context of the named rig, carrying
// the named work bead.
func contextRecordFixture(t *testing.T, townRoot, rig, contextID, workBeadID string, failures int) contextRecord {
	t.Helper()
	dir := filepath.Join(townRoot, rig)
	return contextRecord{
		issue: &beads.Issue{
			ID: contextID,
			Description: beads.FormatSlingContextDescription(&capacity.SlingContextFields{
				Version:          1,
				WorkBeadID:       workBeadID,
				TargetRig:        rig,
				EnqueuedAt:       "2026-04-30T12:00:00Z",
				DispatchFailures: failures,
			}),
		},
		workDir:  dir,
		beadsDir: filepath.Join(dir, ".beads"),
	}
}

// A dispatch cycle reads the open sling contexts once, where each step used to
// read them again. The production scan issues one store read (one bd query)
// per search dir, so the cycle's query count is the rig count rather than a
// multiple of it.
func TestDispatchCycleReadsSlingContextsOnce(t *testing.T) {
	t.Parallel()
	townRoot := townWithRigDirs(t, "alpha", "beta")
	dirs, err := beads.SlingContextSearchDirs(townRoot)
	if err != nil {
		t.Fatalf("search dirs: %v", err)
	}
	if len(dirs) != 3 {
		t.Fatalf("search dirs = %v, want the town root and the two rigs", dirs)
	}

	queries := 0
	store := contextStore{scan: func(string) ([]contextRecord, error) {
		queries += len(dirs) // the production scan reads every search dir once
		return nil, nil
	}}

	for _, cleanup := range []bool{true, false} {
		queries = 0
		if _, err := cycleContexts(townRoot, cleanup, store, nil); err != nil {
			t.Fatalf("cycle (cleanup=%t): %v", cleanup, err)
		}
		if queries != len(dirs) {
			t.Errorf("cycle (cleanup=%t) issued %d sling-context queries, want %d (one per search dir): the cycle reads the contexts once, not once per step",
				cleanup, queries, len(dirs))
		}
	}
}

// The closes a cycle's cleanup makes are applied to that cycle's one read, so
// the steps after the cleanup never see a context it closed.
func TestDispatchCycleSkipsContextsTheCleanupClosed(t *testing.T) {
	t.Parallel()
	townRoot := townWithRigDirs(t, "alpha")
	stale := contextRecordFixture(t, townRoot, "alpha", "gt-wisp-stale", "gt-stale", 0)

	var closed []string
	store := contextStore{
		scan: func(string) ([]contextRecord, error) { return []contextRecord{stale}, nil },
		status: func(string, []string) map[string]beadStatusInfo {
			return map[string]beadStatusInfo{"gt-stale": {Status: "closed"}}
		},
		close: func(rec contextRecord, reason string) error {
			closed = append(closed, rec.issue.ID+" "+reason)
			return nil
		},
	}

	assessments, err := cycleContexts(townRoot, true, store, nil)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if len(closed) != 1 || closed[0] != "gt-wisp-stale stale-work-bead" {
		t.Fatalf("closes = %v, want the stale context closed as stale-work-bead", closed)
	}
	if len(assessments) != 0 {
		t.Fatalf("assessments = %+v, want none: a context the cleanup closed must not reach the later steps", assessments)
	}
}

// The value the steps after the cleanup read is exactly the contexts the
// cleanup left open: the closed one is out, its neighbour stays.
func TestCloseStaleContextsReturnsOnlyTheSurvivors(t *testing.T) {
	t.Parallel()
	townRoot := townWithRigDirs(t, "alpha")
	stale := contextRecordFixture(t, townRoot, "alpha", "gt-wisp-stale", "gt-stale", 0)
	live := contextRecordFixture(t, townRoot, "alpha", "gt-wisp-live", "gt-live", 0)

	var closed []string
	store := contextStore{
		status: func(_ string, ids []string) map[string]beadStatusInfo {
			if len(ids) != 2 {
				t.Fatalf("stale check read %v, want both work beads", ids)
			}
			return map[string]beadStatusInfo{
				"gt-stale": {Status: "closed"},
				"gt-live":  {Status: "open"},
			}
		},
		close: func(rec contextRecord, reason string) error {
			closed = append(closed, rec.issue.ID+" "+reason)
			return nil
		},
	}

	kept := closeStaleContexts(townRoot, []contextRecord{stale, live}, store)

	if len(closed) != 1 || closed[0] != "gt-wisp-stale stale-work-bead" {
		t.Fatalf("closes = %v, want only the stale context", closed)
	}
	if len(kept) != 1 || kept[0].issue.ID != "gt-wisp-live" {
		t.Fatalf("kept = %+v, want only gt-wisp-live", kept)
	}
}

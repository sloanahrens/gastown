package schedulerrun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
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

// cycleQueryLog is a fake bd for one dispatch cycle. It records every call it
// takes, keyed by verb, the way the census counts bd subprocesses; its scan
// issues one list per search dir, which is what the production scan does, so a
// cycle's query count is counted from the calls rather than assumed.
type cycleQueryLog struct {
	events   []string // "<verb> <detail>", in call order
	contexts []contextRecord
	status   map[string]beadStatusInfo
}

func (l *cycleQueryLog) store() contextStore {
	return contextStore{
		scan:   l.scan,
		status: func(string, []string) map[string]beadStatusInfo { return l.status },
		close:  l.closeContext,
		blocked: func(string, []string) ([]*beads.Issue, error) {
			return nil, nil
		},
	}
}

func (l *cycleQueryLog) scan(townRoot string) ([]contextRecord, error) {
	dirs, err := beads.SlingContextSearchDirs(townRoot)
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		l.events = append(l.events, "list "+dir)
	}
	return l.contexts, nil
}

func (l *cycleQueryLog) closeContext(rec contextRecord, reason string) error {
	l.events = append(l.events, "close "+rec.issue.ID+" "+reason)
	return nil
}

// countVerb counts the calls of one verb: the census-style query count.
func (l *cycleQueryLog) countVerb(verb string) int {
	n := 0
	for _, e := range l.events {
		if strings.HasPrefix(e, verb+" ") {
			n++
		}
	}
	return n
}

// writeCapacityTownSettings gives the town a polecat cap, so a cleanup is
// what a cycle is asked to run rather than direct mode skipping it.
func writeCapacityTownSettings(t *testing.T, townRoot string) {
	t.Helper()
	maxPolecats := 2
	writeJSONFile(t, config.TownSettingsPath(townRoot), config.TownSettings{
		Type:      "town-settings",
		Version:   1,
		Scheduler: &capacity.SchedulerConfig{MaxPolecats: &maxPolecats},
	})
}

// A dispatch cycle issues one sling-context query per search dir, where each
// step used to issue its own: the cycle reads the contexts once and the
// cleanup and the plan share that read (gt-pjcpb). The count covers the
// unpaused plan, the paused plan (which skips the cleanup), a cycle with no
// contexts, and the list path.
func TestDispatchCycleQueriesSlingContextsOncePerSearchDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		paused   bool
		list     bool
		contexts bool
	}{
		{name: "plan with cleanup", contexts: true},
		{name: "plan while paused", paused: true, contexts: true},
		{name: "plan with zero contexts"},
		{name: "list", list: true, contexts: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := townWithRigDirs(t, "alpha", "beta")
			dirs, err := beads.SlingContextSearchDirs(townRoot)
			if err != nil {
				t.Fatalf("search dirs: %v", err)
			}
			if len(dirs) != 3 {
				t.Fatalf("search dirs = %v, want the town root and the two rigs", dirs)
			}

			log := &cycleQueryLog{status: map[string]beadStatusInfo{
				"gt-live":  {Status: "open"},
				"gt-stale": {Status: "closed"},
			}}
			if tc.contexts {
				log.contexts = []contextRecord{
					contextRecordFixture(t, townRoot, "alpha", "gt-wisp-stale", "gt-stale", 0),
					contextRecordFixture(t, townRoot, "alpha", "gt-wisp-live", "gt-live", 0),
				}
			}

			if tc.list {
				if _, err := listScheduledWith(townRoot, log.store()); err != nil {
					t.Fatalf("list: %v", err)
				}
			} else {
				writeCapacityTownSettings(t, townRoot)
				if tc.paused {
					if err := capacity.SaveState(townRoot, &capacity.SchedulerState{Paused: true, PausedBy: "test"}); err != nil {
						t.Fatalf("save paused state: %v", err)
					}
				}
				r := &runner{
					opts:     Options{ErrOut: io.Discard},
					deps:     Deps{Seats: func(string, bool) (Seats, error) { return Seats{Free: 1}, nil }},
					contexts: log.store(),
				}
				if _, err := r.buildPlan(townRoot, 0, true); err != nil {
					t.Fatalf("plan: %v", err)
				}
			}

			if got, want := log.countVerb("list"), len(dirs); got != want {
				t.Errorf("cycle issued %d sling-context queries, want %d (one per search dir): the cycle reads the contexts once, not once per step",
					got, want)
			}
			// The cleanup runs after the read, so its close follows the scan it
			// was judged against. A paused cycle closes nothing.
			wantEvents := len(dirs)
			wantClose := ""
			if tc.contexts && !tc.paused && !tc.list {
				wantEvents++
				wantClose = "close gt-wisp-stale stale-work-bead"
			}
			if len(log.events) != wantEvents {
				t.Fatalf("cycle calls = %v, want %d", log.events, wantEvents)
			}
			if wantClose != "" && log.events[len(log.events)-1] != wantClose {
				t.Errorf("cycle calls = %v, want the reads first and then %q", log.events, wantClose)
			}
		})
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

	kept := closeStaleContexts(townRoot, []contextRecord{stale, live}, store, nil)

	if len(closed) != 1 || closed[0] != "gt-wisp-stale stale-work-bead" {
		t.Fatalf("closes = %v, want only the stale context", closed)
	}
	if len(kept) != 1 || kept[0].issue.ID != "gt-wisp-live" {
		t.Fatalf("kept = %+v, want only gt-wisp-live", kept)
	}
}

// A cycle whose read fails reports it, and a cleaning cycle names the cleanup
// it could not do: a plan built on an unreadable context list would miss the
// stale contexts it is meant to close.
func TestDispatchCycleReportsAFailedSlingContextRead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	scanErr := errors.New("dolt unavailable")
	store := contextStore{scan: func(string) ([]contextRecord, error) { return nil, scanErr }}

	for _, cleanup := range []bool{true, false} {
		_, err := cycleContexts(townRoot, cleanup, store, nil)
		if !errors.Is(err, scanErr) {
			t.Fatalf("cycle (cleanup=%t) error = %v, want the scan failure", cleanup, err)
		}
		if cleanup != strings.Contains(err.Error(), "cleaning stale scheduler contexts") {
			t.Fatalf("cycle (cleanup=%t) error = %v, want the cleanup named only when it ran", cleanup, err)
		}
	}
}

// ListScheduled reports the scan it could not make, wrapped, rather than an
// empty queue, which reads as "nothing scheduled". It reports that failure
// once and lists nothing: a partial answer would say "only these are
// scheduled".
func TestListScheduledReportsTheScansOwnFailure(t *testing.T) {
	t.Parallel()
	townRoot := townWithRigDirs(t, "alpha")
	scanErr := errors.New("dolt unavailable")

	got, err := listScheduledWith(townRoot, contextStore{
		scan: func(string) ([]contextRecord, error) { return nil, scanErr },
	})
	if !errors.Is(err, scanErr) {
		t.Fatalf("ListScheduled error = %v, want the injected scan failure", err)
	}
	if got != nil {
		t.Fatalf("ListScheduled = %+v with a failed scan, want nothing listed", got)
	}
	if n := strings.Count(err.Error(), scanErr.Error()); n != 1 {
		t.Fatalf("the scan failure appears %d time(s) in %q, want once", n, err.Error())
	}

	// The failure must be this scan's, not any error: a different one is not
	// reported as the sentinel above.
	otherErr := errors.New("routes unreadable")
	_, err = listScheduledWith(townRoot, contextStore{
		scan: func(string) ([]contextRecord, error) { return nil, otherErr },
	})
	if !errors.Is(err, otherErr) || errors.Is(err, scanErr) {
		t.Fatalf("ListScheduled error = %v, want the scan's own failure only", err)
	}

	// A scan that succeeds reports no failure and nothing scheduled.
	got, err = listScheduledWith(townRoot, contextStore{
		scan: func(string) ([]contextRecord, error) { return nil, nil },
	})
	if err != nil || got != nil {
		t.Fatalf("ListScheduled with nothing scheduled = (%+v, %v), want (nil, nil)", got, err)
	}
}

// A close the cleanup cannot make leaves the context open in the store, so the
// cycle keeps it: dropping it would report the cycle without a context bd
// still has, and the next cycle retries the close. The failure is logged once
// with the context id.
func TestCloseFailureKeepsTheContextInTheCycle(t *testing.T) {
	t.Parallel()
	townRoot := townWithRigDirs(t, "alpha")
	stale := contextRecordFixture(t, townRoot, "alpha", "gt-wisp-stale", "gt-stale", 0)
	closeErr := errors.New("dolt write failed")

	var warnings strings.Builder
	store := contextStore{
		scan: func(string) ([]contextRecord, error) { return []contextRecord{stale}, nil },
		status: func(string, []string) map[string]beadStatusInfo {
			return map[string]beadStatusInfo{"gt-stale": {Status: "closed"}}
		},
		close: func(contextRecord, string) error { return closeErr },
		blocked: func(string, []string) ([]*beads.Issue, error) {
			return nil, nil
		},
	}

	assessments, err := cycleContexts(townRoot, true, store, &warnings)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if len(assessments) != 1 || assessments[0].context.issue.ID != "gt-wisp-stale" {
		t.Fatalf("assessments = %+v, want the context whose close failed to stay in the cycle's read", assessments)
	}
	if got := strings.Count(warnings.String(), "gt-wisp-stale"); got != 1 {
		t.Fatalf("close failure reported %d time(s) in %q, want once with the context id", got, warnings.String())
	}
	if !strings.Contains(warnings.String(), closeErr.Error()) {
		t.Fatalf("warning %q does not name the failure %q", warnings.String(), closeErr)
	}
}

// The same rule for a context whose description does not parse: a close that
// fails leaves it for the next cycle instead of dropping it.
func TestCloseStaleContextsKeepsAnInvalidContextWhoseCloseFailed(t *testing.T) {
	t.Parallel()
	townRoot := townWithRigDirs(t, "alpha")
	invalid := contextRecord{
		issue:    &beads.Issue{ID: "gt-wisp-invalid", Description: "not a sling context"},
		workDir:  filepath.Join(townRoot, "alpha"),
		beadsDir: filepath.Join(townRoot, "alpha", ".beads"),
	}
	store := contextStore{close: func(contextRecord, string) error { return errors.New("dolt write failed") }}

	kept := closeStaleContexts(townRoot, []contextRecord{invalid}, store, io.Discard)
	if len(kept) != 1 || kept[0].issue.ID != "gt-wisp-invalid" {
		t.Fatalf("kept = %+v, want the invalid context whose close failed", kept)
	}
}

// contextScanCounter stands in for one full sling-context scan: it counts the
// scans it is asked to run, which is the bd work the heartbeat would pay.
type contextScanCounter struct {
	calls   int
	records []contextRecord
}

func (c *contextScanCounter) scan(string) ([]contextRecord, error) {
	c.calls++
	return c.records, nil
}

// seedEmptyScan runs one full scan that finds no context, which is what records
// the empty-scan marker the next heartbeat reads.
func seedEmptyScan(t *testing.T, townRoot string, started time.Time, scan *contextScanCounter) {
	t.Helper()
	if _, err := listAllSlingContextRecordsWith(townRoot, func() time.Time { return started }, scan.scan, nil, io.Discard); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if scan.calls != 1 {
		t.Fatalf("first scan ran %d scans, want 1", scan.calls)
	}
}

// While the last full scan found no sling context and nothing has been created
// since, the heartbeat must answer "nothing scheduled" without opening a store:
// that empty scan is the eleven to fourteen bd calls a minute a quiet town pays
// (gt-66e5d).
func TestScanSkipsWhileNoContextCouldExist(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	scan := &contextScanCounter{}

	seedEmptyScan(t, townRoot, start, scan)

	records, err := listAllSlingContextRecordsWith(townRoot,
		func() time.Time { return start.Add(time.Minute) }, scan.scan, nil, io.Discard)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("second scan = %d contexts, want none", len(records))
	}
	if scan.calls != 1 {
		t.Fatalf("a second scan inside ten minutes, with no context created since the empty scan, opened stores again (%d scans); it must answer from the marker", scan.calls)
	}
}

// Every fact the skip rests on, once it stops holding, must send the heartbeat
// back to a full scan. Each row is one of those facts turned against it.
func TestScanReadsAgainAfterStampOrCeiling(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		// firstRecords is what the first full scan found: nil records the empty
		// marker, a context records a marker that found one.
		firstRecords []contextRecord
		// change runs after the first scan and before the second.
		change func(t *testing.T, townRoot string)
		// at is the second scan's clock.
		at time.Time
	}{
		{
			name: "a stamp newer than the empty scan",
			change: func(t *testing.T, townRoot string) {
				if err := beads.TouchSlingContextStamp(townRoot, start.Add(30*time.Second)); err != nil {
					t.Fatalf("touch stamp: %v", err)
				}
			},
			at: start.Add(time.Minute),
		},
		{
			name:   "a marker older than ten minutes",
			change: func(*testing.T, string) {},
			at:     start.Add(11 * time.Minute),
		},
		{
			name:   "a marker at the ten-minute ceiling",
			change: func(*testing.T, string) {},
			at:     start.Add(10 * time.Minute),
		},
		{
			name:         "a marker that found a context",
			firstRecords: []contextRecord{{issue: &beads.Issue{ID: "gt-wisp-ctx"}}},
			change:       func(*testing.T, string) {},
			at:           start.Add(time.Minute),
		},
		{
			name: "a stamp that cannot be read",
			change: func(t *testing.T, townRoot string) {
				// A symlink to itself makes stat fail with ELOOP, which is not
				// "no such file": the scan must not read it as "never created".
				if err := os.Symlink(beads.SlingContextStampFile, beads.SlingContextStampPath(townRoot)); err != nil {
					t.Fatalf("plant unreadable stamp: %v", err)
				}
			},
			at: start.Add(time.Minute),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			scan := &contextScanCounter{records: tc.firstRecords}

			seedEmptyScan(t, townRoot, start, scan)
			tc.change(t, townRoot)
			scan.records = nil

			records, err := listAllSlingContextRecordsWith(townRoot, func() time.Time { return tc.at }, scan.scan, nil, io.Discard)
			if err != nil {
				t.Fatalf("second scan: %v", err)
			}
			if len(records) != 0 {
				t.Fatalf("second scan = %d contexts, want none", len(records))
			}
			if scan.calls != 2 {
				t.Fatalf("%s left the heartbeat answering from its marker (%d scans); it must read every database", tc.name, scan.calls)
			}
		})
	}
}

// TestScanFailureUsesAnInjectedReader: a scan failure reaches the caller rather
// than reading as "nothing scheduled", and the empty-scan marker is left
// unwritten, so the next heartbeat scans again. The bd read is injected: this
// test never runs the production read, which would need a live store.
func TestScanFailureUsesAnInjectedReader(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	scanErr := errors.New("bd down")
	scans := 0
	injected := func(string) ([]contextRecord, error) {
		scans++
		return nil, scanErr
	}

	if _, err := listAllSlingContextRecordsWith(townRoot, func() time.Time { return start }, injected, nil, io.Discard); !errors.Is(err, scanErr) {
		t.Fatalf("listAllSlingContextRecordsWith = %v, want the scan's failure", err)
	}
	if scans != 1 {
		t.Fatalf("the injected read ran %d times, want 1", scans)
	}
	if _, err := os.Stat(beads.SlingContextScanMarkerPath(townRoot)); !os.IsNotExist(err) {
		t.Errorf("a failed scan left a marker (stat: %v); only a completed scan may record one", err)
	}

	// The next heartbeat pays the read again: a failure that recorded an empty
	// scan would hide every context created before the store came back.
	if _, err := listAllSlingContextRecordsWith(townRoot, func() time.Time { return start.Add(time.Minute) }, injected, nil, io.Discard); !errors.Is(err, scanErr) {
		t.Fatalf("second listAllSlingContextRecordsWith = %v, want the scan's failure", err)
	}
	if scans != 2 {
		t.Fatalf("a failed scan left the next heartbeat answering from a marker (%d scans); it must read every database", scans)
	}
}

// A stamp or marker the heartbeat cannot read costs it the skip, and the
// operator must hear that the scan is running in full: the failure is visible
// rather than silently paid for.
func TestListAllSlingContextRecordsWarnsOnUnreadableScanState(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	// An empty-scan marker makes the skip reach the stamp read.
	if err := beads.RecordSlingContextScan(townRoot, start.Add(-time.Minute), false); err != nil {
		t.Fatalf("seed empty-scan marker: %v", err)
	}
	// A symlink to itself makes stat fail with ELOOP, which is not "no such
	// file": the scan must not read it as "never created".
	if err := os.Symlink(beads.SlingContextStampFile, beads.SlingContextStampPath(townRoot)); err != nil {
		t.Fatalf("plant unreadable stamp: %v", err)
	}
	scan := &contextScanCounter{}
	var warned strings.Builder

	records, err := listAllSlingContextRecordsWith(townRoot,
		func() time.Time { return start }, scan.scan, nil, &warned)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("scan = %d contexts, want none", len(records))
	}
	if scan.calls != 1 {
		t.Fatalf("an unreadable stamp left the heartbeat answering from its marker (%d scans); it must read every database", scan.calls)
	}
	if got := warned.String(); !strings.Contains(got, "reading the sling-context scan state failed") {
		t.Errorf("warnings = %q, want one naming the unreadable scan state", got)
	}
}

// An unreadable scan state is warned once per scan ceiling, not on every
// heartbeat: the daemon reads it every minute in one process, and an unwritable
// runtime directory that persists would otherwise repeat the same line every
// cycle until an operator fixes it.
func TestUnreadableScanStateWarnsOncePerCeiling(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := beads.RecordSlingContextScan(townRoot, start.Add(-time.Minute), false); err != nil {
		t.Fatalf("seed empty-scan marker: %v", err)
	}
	if err := os.Symlink(beads.SlingContextStampFile, beads.SlingContextStampPath(townRoot)); err != nil {
		t.Fatalf("plant unreadable stamp: %v", err)
	}
	scan := &contextScanCounter{}
	warnings := func(at time.Time) int {
		t.Helper()
		var warned strings.Builder
		if _, err := listAllSlingContextRecordsWith(townRoot,
			func() time.Time { return at }, scan.scan, nil, &warned); err != nil {
			t.Fatalf("scan at %v: %v", at, err)
		}
		return strings.Count(warned.String(), "reading the sling-context scan state failed")
	}

	if got := warnings(start); got != 1 {
		t.Fatalf("first heartbeat emitted %d unreadable-state warnings, want 1", got)
	}
	if got := warnings(start.Add(time.Minute)); got != 0 {
		t.Errorf("a second heartbeat inside the window emitted %d warnings, want 0: one line per cycle fills the log", got)
	}
	if got := warnings(start.Add(beads.SlingContextScanMaxAge)); got != 1 {
		t.Errorf("a heartbeat at the scan ceiling emitted %d warnings, want 1", got)
	}
}

// A marker the scan cannot write costs the next heartbeat its skip, not this
// scan its answer: the failure is warned about and the records are still
// returned.
func TestListAllSlingContextRecordsWarnsWhenTheScanCannotBeRecorded(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	recordErr := errors.New("disk full")
	scan := &contextScanCounter{records: []contextRecord{{issue: &beads.Issue{ID: "gt-wisp-ctx"}}}}
	record := func(string, time.Time, bool) error { return recordErr }
	var warned strings.Builder

	records, err := listAllSlingContextRecordsWith(townRoot,
		func() time.Time { return start }, scan.scan, record, &warned)
	if err != nil {
		t.Fatalf("scan = %v, want the scan's own answer", err)
	}
	if len(records) != 1 || records[0].issue.ID != "gt-wisp-ctx" {
		t.Fatalf("scan = %+v, want the context the scan read", records)
	}
	if got := warned.String(); !strings.Contains(got, "could not record the sling-context scan") {
		t.Errorf("warnings = %q, want one naming the unwritten marker", got)
	}
}

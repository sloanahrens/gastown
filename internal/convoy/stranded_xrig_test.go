package convoy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// workRigStore is the gastown rig for the stranded scan's blocker check:
// gt-work, blocked by oag-x in the oag rig, and its unblocked sibling gt-sib.
func workRigStore() *fakeStore {
	s := newFakeStore()
	s.seed(
		beads.Issue{ID: "gt-work", Status: "open"},
		beads.Issue{ID: "gt-sib", Status: "open"},
	)
	s.link("gt-work", "external:oag:oag-x", "blocks")
	return s
}

// strandedXrigTown writes a town whose convoy hq-xr tracks gt-work and gt-sib.
// bd show answers both the way bd does for a bead whose blocker is in another
// rig: open, a dependency_count, and no blockers or dependencies listed
// (gt-db8y's shape), so bd alone reads gt-work as unblocked.
func strandedXrigTown(t *testing.T) Town {
	t.Helper()
	routes := `{"prefix":"hq-","path":"."}` + "\n" +
		`{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n" +
		`{"prefix":"oag-","path":"oag/mayor/rig"}` + "\n"
	db := townDB()
	seedConvoy(t, db, beads.Issue{ID: "hq-xr", Title: "Cross-rig convoy"})
	db.Seed(
		beads.Issue{ID: "gt-work", Title: "Work", Priority: 1, Type: "task"},
		beads.Issue{ID: "gt-sib", Title: "Sibling", Priority: 2, Type: "task"},
	)
	rawDepsAnswer(db, map[string][]string{"hq-xr": {"external:gt:gt-work", "external:gt:gt-sib"}})
	return testTown(townWithBeads(t, routes), db, nil)
}

// storeBlockCheck opens the stranded scan's blocker check over the given
// stores, the daemon's wiring: every store is held up front.
func storeBlockCheck(stores map[string]IssueSource) func(string) (blockCheck, func(), error) {
	return func(townRoot string) (blockCheck, func(), error) {
		resolver := NewStoreResolver(townRoot, stores)
		return func(id string) Block {
			return BlockOf(context.Background(), stores["hq"], id, resolver)
		}, func() {}, nil
	}
}

// noBlockers is a blocker check that finds nothing, for scans about something
// other than dependencies.
func noBlockers(string) (blockCheck, func(), error) {
	return func(string) Block { return Block{} }, func() {}, nil
}

// TestFindStrandedConvoys_CrossRigBlockerNotReady is gt-j02xy on the daemon's
// stranded path (review minor 7): gt-work is blocked by oag-x, open in a third
// rig, and bd show drops that blocker. The scan must not offer gt-work as
// ready; when oag-x closes it must.
func TestFindStrandedConvoys_CrossRigBlockerNotReady(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		blocker   string
		wantReady []string
	}{
		{"open", []string{"gt-sib"}},
		{"closed", []string{"gt-work", "gt-sib"}},
	} {
		t.Run(tc.blocker, func(t *testing.T) {
			town := strandedXrigTown(t)
			oag := newFakeStore()
			oag.seed(beads.Issue{ID: "oag-x", Status: tc.blocker})
			check := storeBlockCheck(map[string]IssueSource{"hq": newFakeStore(), "gastown": workRigStore(), "oag": oag})

			stranded, err := town.findStrandedWith(context.Background(), check)
			if err != nil {
				t.Fatalf("findStrandedConvoysWith: %v", err)
			}
			if len(stranded) != 1 {
				t.Fatalf("want 1 stranded convoy, got %+v", stranded)
			}
			if got := strings.Join(stranded[0].ReadyIssues, ","); got != strings.Join(tc.wantReady, ",") {
				t.Errorf("ReadyIssues = %q, want %q", got, strings.Join(tc.wantReady, ","))
			}
		})
	}
}

// TestFindStrandedConvoys_ReportsFailSafeHolds (gt-gg7w9): a bead held because
// its blocker is gone from every rig, or its rig's store will not answer, is
// named in the scan's JSON so the daemon can escalate it. A bead held by an
// open blocker is not: that hold is the tracker working, not a fault.
func TestFindStrandedConvoys_ReportsFailSafeHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		setup     func(oag *fakeStore)
		withOag   bool
		wantCause string
	}{
		{"open blocker", func(oag *fakeStore) { oag.seed(beads.Issue{ID: "oag-x", Status: "open"}) }, true, ""},
		{"dangling blocker", func(*fakeStore) {}, true, "unresolved"},
		{"blocker rig unreachable", func(*fakeStore) {}, false, "unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			town := strandedXrigTown(t)
			oag := newFakeStore()
			tc.setup(oag)
			stores := map[string]IssueSource{"hq": newFakeStore(), "gastown": workRigStore()}
			if tc.withOag {
				stores["oag"] = oag
			}

			stranded, err := town.findStrandedWith(context.Background(), storeBlockCheck(stores))
			if err != nil {
				t.Fatalf("findStrandedConvoysWith: %v", err)
			}
			if len(stranded) != 1 {
				t.Fatalf("want 1 stranded convoy, got %+v", stranded)
			}
			held := stranded[0].Held
			if tc.wantCause == "" {
				if len(held) != 0 {
					t.Errorf("an open blocker is not a fail-safe hold, got %+v", held)
				}
				return
			}
			if len(held) != 1 || held[0].Issue != "gt-work" || held[0].Blocker != "oag-x" || held[0].Cause != tc.wantCause || held[0].Reason == "" {
				t.Errorf("Held = %+v, want gt-work held on oag-x, %s", held, tc.wantCause)
			}
		})
	}
}

// TestFindStrandedConvoys_BlockCheckOpensOnlyWithCandidates: a scan with no
// otherwise-ready bead opens no store.
func TestFindStrandedConvoys_BlockCheckOpensOnlyWithCandidates(t *testing.T) {
	t.Parallel()
	town := testTown(townWithBeads(t, ""), emptyConvoyDB(t, "hq-empty-open", "Empty convoy"), nil)
	opened := 0
	open := func(string) (blockCheck, func(), error) {
		opened++
		return func(string) Block { return Block{} }, func() {}, nil
	}
	if _, err := town.findStrandedWith(context.Background(), open); err != nil {
		t.Fatalf("findStrandedConvoysWith: %v", err)
	}
	if opened != 0 {
		t.Errorf("blocker check opened %d times for a scan with no candidates, want 0", opened)
	}
}

// TestFindStrandedConvoys_TownStoreDownFailsTheScan (review round 2): a town
// store that will not open must fail the scan, not hold every candidate in
// silence. The daemon drops a successful run's stderr, so a silent hold read
// as "N tracked, 0 ready" every scan with no cause; an error makes gt exit
// non-zero and the daemon log "stranded scan failed".
func TestFindStrandedConvoys_TownStoreDownFailsTheScan(t *testing.T) {
	t.Parallel()
	town := strandedXrigTown(t)
	down := func(townRoot string) (blockCheck, func(), error) {
		return openStrandedBlockCheckWith(context.Background(), townRoot, func(string) (IssueSource, error) {
			return nil, errors.New("dial tcp 127.0.0.1:3307: connection refused")
		})
	}

	stranded, err := town.findStrandedWith(context.Background(), down)
	if err == nil {
		t.Fatalf("want an error when the town store will not open, got stranded %+v", stranded)
	}
	for _, want := range []string{"blocker check", "town beads store unavailable", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	for _, s := range stranded {
		if len(s.ReadyIssues) != 0 {
			t.Errorf("convoy %s lists ready issues %v on a failed scan", s.ID, s.ReadyIssues)
		}
	}
}

package convoy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

const rejectedNotes = "MERGE REJECTION (attempt 1): tests fail - see review\nBranch: polecat/x/gt-r+abc"

// TestFeedHold_MergeRejectionIsFlaggedNotHeld pins gt-et7ho: a bead the
// landing worker rejected is ready rework, so the feeders see the rejection
// but no hold; any other hold on the same record still applies.
func TestFeedHold_MergeRejectionIsFlaggedNotHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := holdStore(
		&beads.Issue{ID: "gt-r", Status: "open", Notes: rejectedNotes},
		&beads.Issue{ID: "gt-rpr", Status: "open", Labels: []string{"needs-pro"}, Notes: rejectedNotes},
	)
	if hold := FeedHold(ctx, store, "gt-r", nil); hold != (Hold{MergeRejection: true}) {
		t.Errorf("rejected record: want the rejection flagged with no hold, got %+v", hold)
	}
	if hold := FeedHold(ctx, store, "gt-rpr", nil); hold.Reason != "label needs-pro" || !hold.MergeRejection {
		t.Errorf("rejected record with a routing label: want the label hold and the flag, got %+v", hold)
	}
}

// TestDispatchHoldReason_MergeRejectionIsNotAHold: every dispatcher's hold
// rule leaves a rejected bead free to dispatch.
func TestDispatchHoldReason_MergeRejectionIsNotAHold(t *testing.T) {
	t.Parallel()
	store := holdStore(
		&beads.Issue{ID: "gt-r", Status: "open", Notes: rejectedNotes},
	)
	if reason := DispatchHoldReason(context.Background(), store, "gt-r", nil); reason != "" {
		t.Errorf("a rejected bead must not be held, got %q", reason)
	}
}

// TestFeedHold_NeedsHumanRejectionIsHeld pins gt-hpca9: a rejection the
// landing worker left for a person (gt:needs-human) is not rework, so every
// dispatcher holds it while ordinary rework still flows (gt-et7ho).
func TestFeedHold_NeedsHumanRejectionIsHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := holdStore(
		&beads.Issue{ID: "gt-h", Status: "open", Labels: []string{"gt:needs-human"}, Notes: rejectedNotes},
		&beads.Issue{ID: "gt-hl", Status: "open", Labels: []string{"Needs-Human"}},
		&beads.Issue{ID: "gt-rw", Status: "open", Labels: []string{"rework"}, Notes: rejectedNotes},
	)
	if hold := FeedHold(ctx, store, "gt-h", nil); hold.Reason != "label gt:needs-human" || !hold.MergeRejection {
		t.Errorf("needs-human rejection: want the label hold and the flag, got %+v", hold)
	}
	if reason := DispatchHoldReason(ctx, store, "gt-hl", nil); reason == "" {
		t.Error("a hand-typed needs-human label must hold the bead")
	}
	if reason := DispatchHoldReason(ctx, store, "gt-rw", nil); reason != "" {
		t.Errorf("rework must stay dispatchable, got %q", reason)
	}
}

func TestFeedHold_Verdicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := holdStore(
		&beads.Issue{ID: "gt-clean", Status: "open", Notes: "ordinary notes"},
		&beads.Issue{ID: "gt-deferred", Status: "deferred"},
	)

	if hold := FeedHold(ctx, store, "gt-clean", nil); hold != (Hold{}) {
		t.Errorf("clean record: want no hold, got %+v", hold)
	}

	if hold := FeedHold(ctx, store, "gt-deferred", nil); hold.Reason != "status deferred" || hold.MergeRejection || hold.Unreadable {
		t.Errorf("deferred record: want a plain status hold, got %+v", hold)
	}

	// A record that cannot be read holds the bead: it is the one case where a
	// rejection cannot be ruled out (fail closed).
	broken := &fakeStore{readErr: errors.New("dolt: connection refused")}
	if hold := FeedHold(ctx, broken, "gt-x", nil); !hold.Unreadable || !strings.Contains(hold.Reason, "record unreadable") {
		t.Errorf("unreadable record: want an unreadable hold, got %+v", hold)
	}
	if hold := FeedHold(ctx, store, "gt-missing", nil); !hold.Unreadable || !strings.Contains(hold.Reason, "no record") {
		t.Errorf("missing record: want an unreadable hold, got %+v", hold)
	}

	// No store at all is the town-level gap the store alert reports: no hold.
	if hold := FeedHold(ctx, nil, "gt-x", nil); hold != (Hold{}) {
		t.Errorf("no store: want no hold, got %+v", hold)
	}
}

// TestFeedNextReadyIssue_FeedsRejectedAsRework is the event-driven feed's half
// of gt-et7ho: a sibling close event feeds a bead the landing worker rejected
// like any other ready bead, and one feed dispatches one issue.
func TestFeedNextReadyIssue_FeedsRejectedAsRework(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	convoy := &beads.Issue{ID: "test-convoyr", Title: "Convoy", Status: "open", Priority: 2, Type: "task", CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)}
	// Priority 1 sorts the rejected bead first, so the feed reaches it before
	// the sibling.
	rejected := &beads.Issue{ID: "test-rejected1", Title: "Rejected", Status: "open", Priority: 1, Type: "task", Notes: rejectedNotes, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)}
	fresh := &beads.Issue{ID: "test-fresh1", Title: "Fresh", Status: "open", Priority: 2, Type: "task", CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)}
	for _, iss := range []*beads.Issue{convoy, rejected, fresh} {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}
	for _, id := range []string{rejected.ID, fresh.ID} {
		dep := &depSpec{IssueID: convoy.ID, DependsOnID: id, Type: "tracks", CreatedAt: now.Format(time.RFC3339), CreatedBy: "test"}
		if err := store.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatalf("AddDependency %s: %v", id, err)
		}
	}

	townRoot := setupTownRoot(t)
	gt := &slingLog{}
	logger, logMsgs := makeLogger()

	feedNextReadyIssue(ctx, store, townRoot, convoy.ID, "test", logger, gt.sling, func(string) bool { return false }, nil)

	data, err := gt.read()
	if err != nil {
		t.Fatalf("gt stub was not called (no log file): %v; log: %v", err, *logMsgs)
	}
	got := string(data)
	if !strings.Contains(got, "sling test-rejected1 testrig") {
		t.Errorf("expected the rejected bead to be slung as rework, got %q", got)
	}
	if strings.Contains(got, "test-fresh1") {
		t.Errorf("one feed dispatches one issue, but the sibling was slung too: %q", got)
	}
	for _, m := range *logMsgs {
		if strings.Contains(m, "test-rejected1") && strings.Contains(m, "not dispatched") {
			t.Errorf("rejected bead was held: %q", m)
		}
	}
}

// TestDispatchHoldFields_MatchesTheIssueRule pins the exported field rule
// against the one readHold applies, so the two cannot drift: the witness
// reaches the rule through DispatchHoldFields (it reads a hook bead as JSON
// and holds no IssueSource), and a divergence would mean a polecat
// restarted against work a convoy feeder would have held (gt-n38c6).
func TestDispatchHoldFields_MatchesTheIssueRule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		issue beads.Issue
	}{
		{
			name:  "clean",
			issue: beads.Issue{Status: "open", Notes: "ordinary notes"},
		},
		{
			name:  "label",
			issue: beads.Issue{Status: "open", Labels: []string{"needs-mayor-review"}},
		},
		{
			name:  "label, however typed",
			issue: beads.Issue{Status: "open", Labels: []string{"NEEDS-PRO"}},
		},
		{
			name:  "deferred status",
			issue: beads.Issue{Status: "deferred"},
		},
		{
			name:  "pinned status",
			issue: beads.Issue{Status: "pinned"},
		},
		{
			name:  "decision in design",
			issue: beads.Issue{Status: "open", Design: "## MAYOR DESIGN DECISION\npark it"},
		},
		{
			name:  "decision in notes",
			issue: beads.Issue{Status: "open", Notes: "context\n\n- do not redispatch"},
		},
		{
			name:  "wording only mentioned",
			issue: beads.Issue{Status: "open", Notes: "a review note quoting 'do not redispatch'"},
		},
		{
			name:  "operator label",
			issue: beads.Issue{Status: "open", Labels: []string{"operator"}},
		},
		{
			name:  "operator label, however typed",
			issue: beads.Issue{Status: "open", Labels: []string{"Operator"}},
		},
		{
			name:  "human assignee",
			issue: beads.Issue{Status: "open", Assignee: "sloan"},
		},
		{
			name:  "agent assignee",
			issue: beads.Issue{Status: "open", Assignee: "gastown/polecats/onyx"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fromIssue := dispatchHoldInFields(&tc.issue)
			fromFields := DispatchHoldFields(string(tc.issue.Status), tc.issue.Labels, tc.issue.Assignee, tc.issue.Design, tc.issue.Notes)
			if fromIssue != fromFields {
				t.Errorf("rule drift: dispatchHoldInFields = %q, DispatchHoldFields = %q", fromIssue, fromFields)
			}
		})
	}
}

// TestFeedHold_OperatorReservation pins gt-21pl0: a convoy feeder must not
// re-sling a bead the human operator owns, whether the operator marked it with
// the label or took it by assigning it to themselves.
func TestFeedHold_OperatorReservation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := holdStore(
		&beads.Issue{ID: "gt-op", Status: "open", Labels: []string{"operator"}, Assignee: "sloan"},
		&beads.Issue{ID: "gt-labelled", Status: "open", Labels: []string{"operator"}},
		&beads.Issue{ID: "gt-human", Status: "open", Assignee: "Sloan Ahrens"},
		&beads.Issue{ID: "gt-overseer", Status: "open", Assignee: "overseer"},
		&beads.Issue{ID: "gt-agent", Status: "open", Assignee: "gastown/polecats/onyx"},
		&beads.Issue{ID: "gt-crew", Status: "open", Assignee: "gastown/crew/sloan"},
		&beads.Issue{ID: "gt-unassigned", Status: "open"},
	)

	held := []struct{ id, want string }{
		{"gt-op", "label operator"},
		{"gt-labelled", "label operator"},
		{"gt-human", `assignee Sloan Ahrens is not an agent address`},
		{"gt-overseer", "assignee overseer is not an agent address"},
	}
	for _, tc := range held {
		hold := FeedHold(ctx, store, tc.id, nil)
		if hold.Reason != tc.want {
			t.Errorf("%s: hold = %q, want %q", tc.id, hold.Reason, tc.want)
		}
		if hold.MergeRejection || hold.Unreadable {
			t.Errorf("%s: want a plain reservation hold, got %+v", tc.id, hold)
		}
	}

	// An agent holding the bead is ordinary work in flight, and so is an
	// unassigned bead: neither is the operator's.
	for _, id := range []string{"gt-agent", "gt-crew", "gt-unassigned"} {
		if hold := FeedHold(ctx, store, id, nil); hold != (Hold{}) {
			t.Errorf("%s: want no hold, got %+v", id, hold)
		}
	}
}

// TestDispatchHoldReason_OperatorReservation pins the deacon's half: its
// RECOVERED_BEAD redispatch gates on DispatchHoldReason, and operator work is
// not the deacon's to redispatch either.
func TestDispatchHoldReason_OperatorReservation(t *testing.T) {
	t.Parallel()
	store := holdStore(
		&beads.Issue{ID: "gt-op", Status: "open", Assignee: "sloan"},
	)
	if reason := DispatchHoldReason(context.Background(), store, "gt-op", nil); reason == "" {
		t.Error("deacon path must hold operator work, got no hold")
	}
}

// TestHold_RigStoreNotOpen_ReportsNoHold is gt-2ppfg: a rig whose store never
// opened is the town-level gap the store alert reports, and the hold rule must
// reach that verdict on the bead's own route rather than by reading there. The
// caller holds the town store, which has no record of a rig bead, so the wrong
// store answers "no record" about a bead that has one — the reading that made
// the event-driven feed hold beads the stranded scan fed.
func TestHold_RigStoreNotOpen_ReportsNoHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	townRoot := setupTownRoot(t)
	townStore := holdStore()

	// The daemon's store map has hq but not the "testrig" the bead routes to:
	// a rig that never opened.
	resolver := NewStoreResolver(townRoot, map[string]IssueSource{"hq": townStore})

	// Both feeders' rules, and the deacon's, answer the gap the same way.
	if hold := FeedHold(ctx, townStore, "test-rigbead", resolver); hold != (Hold{}) {
		t.Errorf("a rig whose store is not open must report no hold, got %+v", hold)
	}
	if reason := DispatchHoldReason(ctx, townStore, "test-rigbead", resolver); reason != "" {
		t.Errorf("deacon path: want no hold for a rig with no store, got %q", reason)
	}

	// The gap is the missing store, not a missing record: a bead the town
	// store does own still holds when its record is absent.
	if hold := FeedHold(ctx, townStore, "hq-absent", resolver); !hold.Unreadable || !strings.Contains(hold.Reason, "no record") {
		t.Errorf("an hq bead with no record must still hold, got %+v", hold)
	}
}

// TestHold_RigStoreReadable_StillHoldsUnreadableRecord is the other half of
// gt-2ppfg: a rig store the resolver does have is read, and a record it cannot
// answer for holds the bead. Failing open on the gap must not fail open on a
// store that opened and then could not read.
func TestHold_RigStoreReadable_StillHoldsUnreadableRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	townRoot := setupTownRoot(t)
	townStore := holdStore()
	broken := &fakeStore{readErr: errors.New("dolt: connection refused")}

	resolver := NewStoreResolver(townRoot, map[string]IssueSource{"hq": townStore, "testrig": broken})

	if hold := FeedHold(ctx, townStore, "test-rigbead", resolver); !hold.Unreadable || !strings.Contains(hold.Reason, "record unreadable") {
		t.Errorf("a rig store that cannot read the record must hold it, got %+v", hold)
	}
}

package convoy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
)

// fakeHoldStorage answers the two reads the hold rule makes, and nothing else.
type fakeHoldStorage struct {
	beadsdk.Storage
	issues  map[string]*beadsdk.Issue
	readErr error
}

func (s *fakeHoldStorage) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.issues[id], nil
}

func (s *fakeHoldStorage) GetIssueComments(_ context.Context, _ string) ([]*beadsdk.Comment, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return nil, nil
}

const rejectedNotes = "MERGE REJECTION (attempt 1): tests fail - see review\nBranch: polecat/x/gt-r+abc"

// TestFeedHold_MergeRejection pins gt-ghyfx: a bead the refinery rejected and
// reopened is the deacon's to redispatch, so the convoy feeders' hold names it.
func TestFeedHold_MergeRejection(t *testing.T) {
	store := &fakeHoldStorage{issues: map[string]*beadsdk.Issue{
		"gt-r": {ID: "gt-r", Status: beadsdk.StatusOpen, Notes: rejectedNotes},
	}}
	hold := FeedHold(context.Background(), store, "gt-r", nil)
	if !hold.MergeRejection || hold.Unreadable {
		t.Fatalf("want a merge-rejection hold, got %+v", hold)
	}
	if !strings.Contains(hold.Reason, "merge rejection") {
		t.Errorf("reason should name the merge rejection, got %q", hold.Reason)
	}
}

// TestDispatchHoldReason_MergeRejectionIsNotAHold pins the other side of
// gt-ghyfx: the deacon's RECOVERED_BEAD redispatch gates on DispatchHoldReason
// and exists to redispatch rejected beads, so the marker must not hold there.
func TestDispatchHoldReason_MergeRejectionIsNotAHold(t *testing.T) {
	store := &fakeHoldStorage{issues: map[string]*beadsdk.Issue{
		"gt-r": {ID: "gt-r", Status: beadsdk.StatusOpen, Notes: rejectedNotes},
	}}
	if reason := DispatchHoldReason(context.Background(), store, "gt-r", nil); reason != "" {
		t.Errorf("deacon path must not hold a rejected bead, got %q", reason)
	}
}

func TestFeedHold_Verdicts(t *testing.T) {
	ctx := context.Background()
	store := &fakeHoldStorage{issues: map[string]*beadsdk.Issue{
		"gt-clean":    {ID: "gt-clean", Status: beadsdk.StatusOpen, Notes: "ordinary notes"},
		"gt-deferred": {ID: "gt-deferred", Status: beadsdk.StatusDeferred},
	}}

	if hold := FeedHold(ctx, store, "gt-clean", nil); hold != (Hold{}) {
		t.Errorf("clean record: want no hold, got %+v", hold)
	}

	if hold := FeedHold(ctx, store, "gt-deferred", nil); hold.Reason != "status deferred" || hold.MergeRejection || hold.Unreadable {
		t.Errorf("deferred record: want a plain status hold, got %+v", hold)
	}

	// A record that cannot be read holds the bead: it is the one case where a
	// rejection cannot be ruled out (fail closed).
	broken := &fakeHoldStorage{readErr: errors.New("dolt: connection refused")}
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

// TestFeedNextReadyIssue_SkipsRejectedFeedsSibling is the case gt-ghyfx asks
// for on the event-driven feed: a sibling close event must not re-sling a bead
// the refinery rejected, while a fresh sibling still feeds.
func TestFeedNextReadyIssue_SkipsRejectedFeedsSibling(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	convoy := &beadsdk.Issue{ID: "test-convoyr", Title: "Convoy", Status: beadsdk.StatusOpen, Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now}
	// Priority 1 sorts the rejected bead first, so skipping it is what lets
	// the sibling feed.
	rejected := &beadsdk.Issue{ID: "test-rejected1", Title: "Rejected", Status: beadsdk.StatusOpen, Priority: 1, IssueType: beadsdk.TypeTask, Notes: rejectedNotes, CreatedAt: now, UpdatedAt: now}
	fresh := &beadsdk.Issue{ID: "test-fresh1", Title: "Fresh", Status: beadsdk.StatusOpen, Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now}
	for _, iss := range []*beadsdk.Issue{convoy, rejected, fresh} {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}
	for _, id := range []string{rejected.ID, fresh.ID} {
		dep := &beadsdk.Dependency{IssueID: convoy.ID, DependsOnID: id, Type: beadsdk.DependencyType("tracks"), CreatedAt: now, CreatedBy: "test"}
		if err := store.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatalf("AddDependency %s: %v", id, err)
		}
	}

	townRoot := setupTownRoot(t)
	gtPath, logPath := makeGTStub(t, 0)
	logger, logMsgs := makeLogger()

	feedNextReadyIssue(ctx, store, townRoot, convoy.ID, "test", logger, gtPath, func(string) bool { return false }, nil)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("gt stub was not called (no log file): %v; log: %v", err, *logMsgs)
	}
	got := string(data)
	if strings.Contains(got, "test-rejected1") {
		t.Errorf("rejected bead was slung by the event feed: %q", got)
	}
	if !strings.Contains(got, "sling test-fresh1 testrig") {
		t.Errorf("expected the fresh sibling to be slung, got %q", got)
	}
	found := false
	for _, m := range *logMsgs {
		if strings.Contains(m, "test-rejected1") && strings.Contains(m, "merge rejection") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a log line naming the merge rejection on test-rejected1, got %v", *logMsgs)
	}
}

package convoy

import (
	"context"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/testutil"
)

// setupTestStore opens a real beads database in a temp dir for integration tests.
// Fails the test if the store cannot be opened: a skip would hide lost coverage.
// Caller must run the returned cleanup when done.
func setupTestStore(t *testing.T) (beadsdk.Storage, func()) {
	t.Helper()

	// BEADS_TEST_MODE is set once, process-wide, by TestMain — not here — so
	// that this helper's callers can call t.Parallel() (see testmain_test.go).

	ctx := context.Background()
	// Fails, never skips, on an open error: a skipped store test is lost
	// coverage with no red signal.
	store := testutil.OpenTestStore(t, ctx)

	if err := store.SetConfig(ctx, "issue_prefix", "test"); err != nil {
		t.Fatalf("SetConfig issue_prefix: %v", err)
	}

	cleanup := func() {
		_ = store.Close()
	}
	return store, cleanup
}

func TestSetupTestStore_OpensStore(t *testing.T) {
	store, cleanup := setupTestStore(t)
	t.Parallel()
	defer cleanup()

	if store == nil {
		t.Fatal("setupTestStore returned nil store")
	}
}

func TestGetTrackingConvoys_FiltersByTracksType(t *testing.T) {
	store, cleanup := setupTestStore(t)
	t.Parallel()
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create convoy and tracked issue
	convoyIssue := &beadsdk.Issue{
		ID:        "hq-cv-test1",
		Title:     "Test Convoy",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	trackedIssue := &beadsdk.Issue{
		ID:        "gt-tracked1",
		Title:     "Tracked",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.CreateIssue(ctx, convoyIssue, "test"); err != nil {
		t.Fatalf("CreateIssue convoy: %v", err)
	}
	if err := store.CreateIssue(ctx, trackedIssue, "test"); err != nil {
		t.Fatalf("CreateIssue tracked: %v", err)
	}

	// Add tracks dependency: convoy tracks issue (convoy depends on issue with type tracks)
	dep := &beadsdk.Dependency{
		IssueID:     convoyIssue.ID,
		DependsOnID: trackedIssue.ID,
		Type:        beadsdk.DependencyType("tracks"),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	// Add blocks dependency (should be filtered out)
	otherIssue := &beadsdk.Issue{
		ID:        "gt-other",
		Title:     "Other",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, otherIssue, "test"); err != nil {
		t.Fatalf("CreateIssue other: %v", err)
	}
	blocksDep := &beadsdk.Dependency{
		IssueID:     "hq-cv-other",
		DependsOnID: trackedIssue.ID,
		Type:        beadsdk.DepBlocks,
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.CreateIssue(ctx, &beadsdk.Issue{
		ID:        "hq-cv-other",
		Title:     "Other Convoy",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}, "test"); err != nil {
		t.Fatalf("CreateIssue other convoy: %v", err)
	}
	if err := store.AddDependency(ctx, blocksDep, "test"); err != nil {
		t.Fatalf("AddDependency blocks: %v", err)
	}

	// getTrackingConvoys(trackedIssue.ID) should return only hq-cv-test1 (tracks), not hq-cv-other (blocks)
	convoyIDs := getTrackingConvoys(ctx, store, trackedIssue.ID, nil)
	if len(convoyIDs) != 1 || convoyIDs[0] != convoyIssue.ID {
		t.Errorf("getTrackingConvoys = %v, want [%s]", convoyIDs, convoyIssue.ID)
	}
}

func TestIsConvoyClosed_ReturnsCorrectStatus(t *testing.T) {
	store, cleanup := setupTestStore(t)
	t.Parallel()
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	openIssue := &beadsdk.Issue{
		ID:        "hq-cv-open",
		Title:     "Open",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	closedIssue := &beadsdk.Issue{
		ID:        "hq-cv-closed",
		Title:     "Closed",
		Status:    beadsdk.StatusClosed,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.CreateIssue(ctx, openIssue, "test"); err != nil {
		t.Fatalf("CreateIssue open: %v", err)
	}
	if err := store.CreateIssue(ctx, closedIssue, "test"); err != nil {
		t.Fatalf("CreateIssue closed: %v", err)
	}

	if isConvoyClosed(ctx, store, openIssue.ID) {
		t.Error("isConvoyClosed(open) = true, want false")
	}
	if !isConvoyClosed(ctx, store, closedIssue.ID) {
		t.Error("isConvoyClosed(closed) = false, want true")
	}
}

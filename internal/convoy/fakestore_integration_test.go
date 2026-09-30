//go:build integration

package convoy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/testutil"
)

// buildParityTown writes one convoy's world into store: test-cv tracks
// test-ready, test-blocked (blocked by the open test-blk), test-done, the
// merge-blocked test-mb1 and test-mb2, and the cross-rig external:oag:oag-x.
// test-held carries a hold label; test-child is test-blk's child, which does
// not block; test-tomb's merge blocker became a tombstone.
func buildParityTown(t *testing.T, store beadsdk.Storage) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	issue := func(id string, status beadsdk.Status, priority int, closeReason string) {
		t.Helper()
		if err := store.CreateIssue(ctx, &beadsdk.Issue{
			ID: id, Title: id, Status: status, Priority: priority, CloseReason: closeReason,
			IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
		}, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", id, err)
		}
	}
	edge := func(from, to, depType string) {
		t.Helper()
		if err := store.AddDependency(ctx, &beadsdk.Dependency{
			IssueID: from, DependsOnID: to, Type: beadsdk.DependencyType(depType), CreatedAt: now, CreatedBy: "test",
		}, "test"); err != nil {
			t.Fatalf("AddDependency %s -> %s: %v", from, to, err)
		}
	}

	issue("test-cv", beadsdk.StatusOpen, 2, "")
	issue("test-held", beadsdk.StatusOpen, 0, "")
	issue("test-blocked", beadsdk.StatusOpen, 1, "")
	issue("test-ready", beadsdk.StatusOpen, 2, "")
	issue("test-done", beadsdk.StatusClosed, 2, "")
	issue("test-blk", beadsdk.StatusOpen, 2, "")
	issue("test-child", beadsdk.StatusOpen, 2, "")
	issue("test-mbr1", beadsdk.StatusClosed, 2, "")
	issue("test-mb1", beadsdk.StatusOpen, 3, "")
	issue("test-mbr2", beadsdk.StatusClosed, 2, "Merged in mr-xyz")
	issue("test-mb2", beadsdk.StatusOpen, 3, "")
	issue("test-tblk", beadsdk.StatusOpen, 2, "")
	issue("test-tomb", beadsdk.StatusOpen, 3, "")

	for _, id := range []string{"test-held", "test-blocked", "test-ready", "test-done", "test-mb1", "test-mb2", "external:oag:oag-x"} {
		edge("test-cv", id, "tracks")
	}
	edge("test-blocked", "test-blk", "blocks")
	edge("test-child", "test-blk", "parent-child")
	edge("test-mb1", "test-mbr1", "merge-blocks")
	edge("test-mb2", "test-mbr2", "merge-blocks")
	edge("test-tomb", "test-tblk", "merge-blocks")
	if err := store.UpdateIssue(ctx, "test-tblk", map[string]interface{}{"status": "tombstone"}, "test"); err != nil {
		t.Fatalf("UpdateIssue tombstone: %v", err)
	}
	if err := store.AddLabel(ctx, "test-held", "needs-mayor-review", "test"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
}

// observeParityTown is everything the convoy code reads from the world
// buildParityTown writes, as one comparable string per read.
func observeParityTown(t *testing.T, store beadsdk.Storage, townRoot string) []string {
	t.Helper()
	ctx := context.Background()
	var obs []string

	for _, id := range []string{"test-ready", "oag-x", "test-blk"} {
		obs = append(obs, fmt.Sprintf("tracking %s: %v", id, getTrackingConvoys(ctx, store, townRoot, id, nil)))
	}
	ids, err := trackedIDs(ctx, store, "test-cv")
	sort.Strings(ids)
	obs = append(obs, fmt.Sprintf("trackedIDs: %v %v", ids, err))
	for _, id := range []string{"test-ready", "test-blocked", "test-child", "test-mb1", "test-mb2", "test-tomb"} {
		obs = append(obs, fmt.Sprintf("blocked %s: %v", id, isIssueBlocked(ctx, store, id, nil)))
	}
	held, err := store.GetIssue(ctx, "test-held")
	if err != nil {
		t.Fatalf("GetIssue test-held: %v", err)
	}
	obs = append(obs, fmt.Sprintf("labels: %v", held.Labels))
	tracked := getConvoyTrackedIssues(ctx, store, "test-cv", townRoot, NewStoreResolver(townRoot, map[string]beadsdk.Storage{}), func(string, ...interface{}) {})
	sort.Slice(tracked, func(i, j int) bool { return tracked[i].ID < tracked[j].ID })
	for _, tr := range tracked {
		obs = append(obs, fmt.Sprintf("tracked %s: %q", tr.ID, tr.Status))
	}

	gt := &slingLog{}
	feedNextReadyIssue(ctx, store, townRoot, "test-cv", "parity", func(string, ...interface{}) {}, gt.sling, func(string) bool { return false }, nil)
	slung, _ := gt.read()
	obs = append(obs, "slung: "+string(slung))
	return obs
}

// TestIntegrationFakeStoreMatchesDolt pins fakeRigStore, the store every unit
// test in this package runs on, to the Dolt store: the same writes must read
// back the same through every read the convoy code makes.
func TestIntegrationFakeStoreMatchesDolt(t *testing.T) {
	t.Parallel()
	townRoot := setupTownRoot(t)
	routes := `{"prefix":"test-","path":"testrig/.beads"}` + "\n" + `{"prefix":"oag-","path":"oag/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}

	dolt := testutil.OpenTestStore(t, context.Background())
	if err := dolt.SetConfig(context.Background(), "issue_prefix", "test"); err != nil {
		t.Fatalf("SetConfig issue_prefix: %v", err)
	}
	fake, _ := setupTestStore(t)

	buildParityTown(t, dolt)
	buildParityTown(t, fake)

	want := observeParityTown(t, dolt, townRoot)
	got := observeParityTown(t, fake, townRoot)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fake store reads differ from Dolt:\n fake: %q\n dolt: %q", got, want)
	}
	t.Logf("dolt reads: %q", want)
}

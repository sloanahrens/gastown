package convoy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// emptyConvoyDB is a town database holding one open convoy that tracks
// nothing.
func emptyConvoyDB(t *testing.T, convoyID, convoyTitle string) *beadsfake.Fake {
	db := townDB()
	seedConvoy(t, db, beads.Issue{ID: convoyID, Title: convoyTitle})
	return db
}

// mustStatus fails unless id is in status in db.
func mustStatus(t *testing.T, db beads.Client, id, status string) {
	t.Helper()
	got, err := db.Show(id)
	if err != nil {
		t.Fatalf("Show(%s): %v", id, err)
	}
	if got.Status != status {
		t.Errorf("%s is %s, want %s", id, got.Status, status)
	}
}

// townWithBeads is an empty town root holding a .beads directory, with
// routes when routes is not empty.
func townWithBeads(t *testing.T, routes string) string {
	t.Helper()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if routes != "" {
		if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routes), 0644); err != nil {
			t.Fatalf("write routes: %v", err)
		}
	}
	return townRoot
}

func TestCheckSingleConvoy_EmptyConvoyDoesNotAutoClose(t *testing.T) {
	t.Parallel()
	// When getTrackedIssues returns empty (cross-rig resolution failure or truly
	// no tracked issues), closeConvoyIfComplete must NOT auto-close. A 0/0 result
	// means "could not resolve", not "all done". (GH#hq-439)
	db := emptyConvoyDB(t, "hq-empty1", "Empty test convoy")

	err := testTown(townWithBeads(t, ""), db, &gtScript{}).CheckOne("hq-empty1", false)
	if err != nil {
		t.Fatalf("checkSingleConvoy() error: %v", err)
	}

	// 0 tracked issues = no auto-close.
	mustStatus(t, db, "hq-empty1", "open")
}

func TestCheckSingleConvoy_EmptyConvoyDryRun(t *testing.T) {
	t.Parallel()
	db := emptyConvoyDB(t, "hq-empty2", "Dry run convoy")

	err := testTown(townWithBeads(t, ""), db, &gtScript{}).CheckOne("hq-empty2", true)
	if err != nil {
		t.Fatalf("checkSingleConvoy() dry-run error: %v", err)
	}
	mustStatus(t, db, "hq-empty2", "open")
}

func TestFindStrandedConvoys_EmptyConvoyFlagged(t *testing.T) {
	t.Parallel()
	db := emptyConvoyDB(t, "hq-empty3", "Stranded empty convoy")

	stranded, err := testTown(townWithBeads(t, ""), db, nil).findStrandedWith(context.Background(), noBlockers)
	if err != nil {
		t.Fatalf("findStrandedConvoys() error: %v", err)
	}

	if len(stranded) != 1 {
		t.Fatalf("expected 1 stranded convoy, got %d", len(stranded))
	}

	s := stranded[0]
	if s.ID != "hq-empty3" {
		t.Errorf("stranded convoy ID = %q, want %q", s.ID, "hq-empty3")
	}
	if s.ReadyCount != 0 {
		t.Errorf("stranded ReadyCount = %d, want 0", s.ReadyCount)
	}
	if len(s.ReadyIssues) != 0 {
		t.Errorf("stranded ReadyIssues = %v, want empty", s.ReadyIssues)
	}
}

// TestFindStrandedConvoys_MixedConvoys verifies that findStrandedConvoys
// correctly returns both empty (cleanup) and feedable (has ready issues)
// convoys, and that the JSON output shape is correct for each type.
func TestFindStrandedConvoys_MixedConvoys(t *testing.T) {
	t.Parallel()
	// Routes needed so isSlingableBead can resolve gt- prefix to a rig
	townRoot := townWithBeads(t, `{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n")

	// Two convoys: one empty, one tracking a ready issue.
	db := townDB()
	seedConvoy(t, db, beads.Issue{ID: "hq-empty-mix", Title: "Empty convoy"})
	seedConvoy(t, db, beads.Issue{ID: "hq-feed-mix", Title: "Feedable convoy"},
		beads.Issue{ID: "gt-ready1", Title: "Ready issue", Type: "task"})

	// Pass townRoot (not .beads) — matches getTownBeadsDir() which returns the workspace root.
	stranded, err := testTown(townRoot, db, nil).findStrandedWith(context.Background(), noBlockers)
	if err != nil {
		t.Fatalf("findStrandedConvoys() error: %v", err)
	}

	if len(stranded) != 2 {
		t.Fatalf("expected 2 stranded convoys, got %d", len(stranded))
	}

	// Build a map for easier assertions
	byID := map[string]StrandedConvoy{}
	for _, s := range stranded {
		byID[s.ID] = s
	}

	// Verify empty convoy
	empty, ok := byID["hq-empty-mix"]
	if !ok {
		t.Fatal("missing empty convoy hq-empty-mix in stranded results")
	}
	if empty.ReadyCount != 0 {
		t.Errorf("empty convoy ReadyCount = %d, want 0", empty.ReadyCount)
	}
	if empty.TrackedCount != 0 {
		t.Errorf("empty convoy TrackedCount = %d, want 0", empty.TrackedCount)
	}
	if len(empty.ReadyIssues) != 0 {
		t.Errorf("empty convoy ReadyIssues = %v, want empty", empty.ReadyIssues)
	}

	// Verify feedable convoy
	feedable, ok := byID["hq-feed-mix"]
	if !ok {
		t.Fatal("missing feedable convoy hq-feed-mix in stranded results")
	}
	if feedable.ReadyCount != 1 {
		t.Errorf("feedable convoy ReadyCount = %d, want 1", feedable.ReadyCount)
	}
	if feedable.TrackedCount != 1 {
		t.Errorf("feedable convoy TrackedCount = %d, want 1", feedable.TrackedCount)
	}
	if len(feedable.ReadyIssues) != 1 || feedable.ReadyIssues[0] != "gt-ready1" {
		t.Errorf("feedable convoy ReadyIssues = %v, want [gt-ready1]", feedable.ReadyIssues)
	}

	// Verify JSON encoding shape — empty slice encodes as [] not null
	jsonBytes, err := json.Marshal(stranded)
	if err != nil {
		t.Fatalf("json.Marshal(stranded): %v", err)
	}
	jsonStr := string(jsonBytes)
	if strings.Contains(jsonStr, `"ready_issues":null`) {
		t.Error("JSON output contains ready_issues:null — should be [] for empty convoys")
	}
	// Verify tracked_count appears in JSON
	if !strings.Contains(jsonStr, `"tracked_count"`) {
		t.Error("JSON output missing tracked_count field")
	}
}

// TestFindStrandedConvoys_StuckConvoy verifies that a convoy with tracked
// issues but none ready (stuck) is included in the stranded list with
// TrackedCount > 0 and ReadyCount == 0, preventing accidental auto-close.
func TestFindStrandedConvoys_StuckConvoy(t *testing.T) {
	t.Parallel()
	townRoot := townWithBeads(t, `{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n")

	// The convoy's tracked issues are all blocked — none are ready.
	db := townDB()
	busy := func(id, title string) beads.Issue {
		return beads.Issue{ID: id, Title: title, Type: "task", BlockedBy: []string{"gt-blocker1"}, BlockedByCount: 1}
	}
	seedConvoy(t, db, beads.Issue{ID: "hq-stuck1", Title: "Stuck convoy"},
		busy("gt-busy1", "Blocked issue 1"), busy("gt-busy2", "Blocked issue 2"))

	// Both beads are blocked by gt-blocker1, as the blocker check reports.
	blockedByBlocker1 := func(string) (blockCheck, func(), error) {
		return func(id string) Block {
			if strings.HasPrefix(id, "gt-busy") {
				return Block{Reason: "blocks gt-blocker1 (open)"}
			}
			return Block{}
		}, func() {}, nil
	}
	stranded, err := testTown(townRoot, db, nil).findStrandedWith(context.Background(), blockedByBlocker1)
	if err != nil {
		t.Fatalf("findStrandedConvoys() error: %v", err)
	}

	if len(stranded) != 1 {
		t.Fatalf("expected 1 stranded convoy (stuck), got %d", len(stranded))
	}

	s := stranded[0]
	if s.ID != "hq-stuck1" {
		t.Errorf("stranded convoy ID = %q, want %q", s.ID, "hq-stuck1")
	}
	if s.TrackedCount != 2 {
		t.Errorf("stuck convoy TrackedCount = %d, want 2", s.TrackedCount)
	}
	if s.ReadyCount != 0 {
		t.Errorf("stuck convoy ReadyCount = %d, want 0", s.ReadyCount)
	}
}

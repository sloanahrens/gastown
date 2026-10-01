package convoy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// emptyConvoyBd answers the bd calls checkSingleConvoy and
// findStrandedConvoys make for one open convoy that tracks nothing.
func emptyConvoyBd(convoyID, convoyTitle string) *bdScript {
	return &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		switch positional(c.Args)[0] {
		case "show":
			return `[{"id":"` + convoyID + `","title":"` + convoyTitle + `","status":"open","issue_type":"convoy"}]`, "", 0
		case "sql", "dep":
			return "[]", "", 0
		case "list":
			return `[{"id":"` + convoyID + `","title":"` + convoyTitle + `"}]`, "", 0
		}
		return "", "", 0
	}}
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
	bd := emptyConvoyBd("hq-empty1", "Empty test convoy")

	err := testTown(townWithBeads(t, ""), bd, &gtScript{}).CheckOne("hq-empty1", false)
	if err != nil {
		t.Fatalf("checkSingleConvoy() error: %v", err)
	}

	// Verify bd close was NOT called — 0 tracked issues = no auto-close.
	if closes := bd.ran("close"); len(closes) != 0 {
		t.Errorf("convoy with 0 tracked issues should NOT be auto-closed, but bd close ran: %v", closes)
	}
}

func TestCheckSingleConvoy_EmptyConvoyDryRun(t *testing.T) {
	t.Parallel()
	bd := emptyConvoyBd("hq-empty2", "Dry run convoy")

	err := testTown(townWithBeads(t, ""), bd, &gtScript{}).CheckOne("hq-empty2", true)
	if err != nil {
		t.Fatalf("checkSingleConvoy() dry-run error: %v", err)
	}

	// In dry-run mode, bd close should NOT be called
	if closes := bd.ran("close"); len(closes) != 0 {
		t.Errorf("dry-run should not call bd close, but it ran: %v", closes)
	}
}

func TestFindStrandedConvoys_EmptyConvoyFlagged(t *testing.T) {
	t.Parallel()
	bd := emptyConvoyBd("hq-empty3", "Stranded empty convoy")

	stranded, err := testTown(townWithBeads(t, ""), bd, nil).findStrandedWith(context.Background(), noBlockers)
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

	// bd returns two convoys: one empty, one with a ready issue.
	bd := &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		pos := positional(c.Args)
		switch pos[0] {
		case "list":
			return `[{"id":"hq-empty-mix","title":"Empty convoy"},{"id":"hq-feed-mix","title":"Feedable convoy"}]`, "", 0
		case "sql":
			// DepListRawIDs: beadsql.RawDeps(<id>, "down", "tracks")
			if strings.Contains(strings.Join(c.Args, " "), "issue_id = 'hq-feed-mix'") {
				return `[{"depends_on_id":"gt-ready1"}]`, "", 0
			}
			return "[]", "", 0
		case "dep":
			if len(pos) > 2 && pos[2] == "hq-feed-mix" {
				return `[{"id":"gt-ready1","title":"Ready issue","status":"open","issue_type":"task","assignee":"","dependency_type":"tracks"}]`, "", 0
			}
			return "[]", "", 0
		case "show":
			return `[{"id":"gt-ready1","title":"Ready issue","status":"open","issue_type":"task","assignee":"","blocked_by":[],"blocked_by_count":0,"dependencies":[]}]`, "", 0
		case "query":
			// The fork's --json query prints "[]" for no rows, never nothing.
			return "[]", "", 0
		}
		return "", "", 0
	}}

	// Pass townRoot (not .beads) — matches getTownBeadsDir() which returns the workspace root.
	stranded, err := testTown(townRoot, bd, nil).findStrandedWith(context.Background(), noBlockers)
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

	// bd: convoy has tracked issues but all are blocked — none are ready.
	bd := &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		switch positional(c.Args)[0] {
		case "list":
			return `[{"id":"hq-stuck1","title":"Stuck convoy"}]`, "", 0
		case "sql":
			return `[{"depends_on_id":"gt-busy1"},{"depends_on_id":"gt-busy2"}]`, "", 0
		case "dep":
			return `[{"id":"gt-busy1","title":"Blocked issue 1","status":"open","issue_type":"task","assignee":"","dependency_type":"tracks"},{"id":"gt-busy2","title":"Blocked issue 2","status":"open","issue_type":"task","assignee":"","dependency_type":"tracks"}]`, "", 0
		case "show":
			// Both issues have blockers so isReadyIssue returns false
			return `[{"id":"gt-busy1","title":"Blocked issue 1","status":"open","issue_type":"task","assignee":"","blocked_by":["gt-blocker1"],"blocked_by_count":1,"dependencies":[]},{"id":"gt-busy2","title":"Blocked issue 2","status":"open","issue_type":"task","assignee":"","blocked_by":["gt-blocker1"],"blocked_by_count":1,"dependencies":[]}]`, "", 0
		}
		return "", "", 0
	}}

	// Both beads are blocked by gt-blocker1, as the blocker check reports.
	blockedByBlocker1 := func(string) (blockCheck, func(), error) {
		return func(id string) Block {
			if strings.HasPrefix(id, "gt-busy") {
				return Block{Reason: "blocks gt-blocker1 (open)"}
			}
			return Block{}
		}, func() {}, nil
	}
	stranded, err := testTown(townRoot, bd, nil).findStrandedWith(context.Background(), blockedByBlocker1)
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

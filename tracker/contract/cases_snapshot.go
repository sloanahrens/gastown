package contract

import (
	"errors"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "the town snapshot pages with exact counts and carries the blocked verdict", Run: snapshotPageAndCounts})
}

func mustSnapshot(t *testing.T, s store.Store, req api.SnapshotRequest) api.TownSnapshot {
	t.Helper()
	snap, err := s.TownSnapshot(ctx(), req)
	if err != nil {
		t.Fatalf("TownSnapshot(%+v): %v", req, err)
	}
	return snap
}

func snapshotIDs(snap api.TownSnapshot) []string {
	out := make([]string, len(snap.Issues))
	for i, is := range snap.Issues {
		out[i] = is.ID
	}
	return out
}

// snapshotPageAndCounts pins one page's contract: list order, the cut flag
// and cursor, counts over the whole set rather than the page, and the blocked
// verdict with its direct blockers. Each page is read at its own moment, so
// the walk asserts the page contents, not one At across pages.
func snapshotPageAndCounts(t *testing.T, s store.Store) {
	blocker := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "blocker", Priority: 1})
	blocked := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "blocked", Priority: 2})
	if err := s.AddDependency(ctx(), blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	third := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "third", Priority: 3, Labels: []string{"x"}})
	hq := mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "hq fourth", Priority: 4})
	// A wisp is live work, not part of the town's durable counts.
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "wisp", Ephemeral: true, Priority: 5})

	all := mustSnapshot(t, s, api.SnapshotRequest{})
	if all.Truncated || all.NextCursor != "" {
		t.Errorf("whole snapshot truncated=%v cursor=%q, want the whole set in one page", all.Truncated, all.NextCursor)
	}
	if all.At.IsZero() {
		t.Error("At is zero; the answer must say when it was read")
	}
	sameSet(t, "the snapshot's page", snapshotIDs(all), blocker.ID, blocked.ID, third.ID, hq.ID)

	counts := all.Counts
	if counts.Total != 4 || counts.ByStatus["open"] != 4 || counts.ByRig["gt"] != 3 || counts.ByRig["hq"] != 1 {
		t.Errorf("counts = %+v, want 4 open durable issues (3 gt, 1 hq) and the wisp uncounted", counts)
	}
	if counts.Blocked != 1 || counts.BlockedRigs["gt"] != 1 {
		t.Errorf("blocked counts = %d, rigs %v; want the one blocked gt issue", counts.Blocked, counts.BlockedRigs)
	}
	for _, is := range all.Issues {
		switch is.ID {
		case third.ID:
			if is.Blocked || len(is.BlockedBy) != 0 {
				t.Errorf("unblocked row = blocked:%v by:%v, want neither", is.Blocked, is.BlockedBy)
			}
			if !slices.Equal(is.Labels, []string{"x"}) || is.Title != "third" || is.CreatedAt.IsZero() {
				t.Errorf("labeled row = %+v, want the full list row with its labels", is)
			}
		case blocked.ID:
			if !is.Blocked || !slices.Equal(is.BlockedBy, []string{blocker.ID}) {
				t.Errorf("blocked row = blocked:%v by:%v, want blocked by %s", is.Blocked, is.BlockedBy, blocker.ID)
			}
		}
	}

	// A cut page says so, carries the cursor, and still reports the exact
	// totals for the whole set.
	first := mustSnapshot(t, s, api.SnapshotRequest{Limit: 2})
	if !first.Truncated || first.NextCursor == "" {
		t.Fatalf("page of 2 truncated=%v cursor=%q, want both set", first.Truncated, first.NextCursor)
	}
	if len(first.Issues) != 2 || first.Issues[0].ID != blocker.ID || first.Issues[1].ID != blocked.ID {
		t.Errorf("page of 2 = %v, want the first two in priority order", snapshotIDs(first))
	}
	if first.Counts.Total != 4 || first.Counts.ByRig["hq"] != 1 {
		t.Errorf("cut page counts = %+v, want the whole set's exact totals", first.Counts)
	}

	var walked []string
	cursor := ""
	for pages := 0; ; pages++ {
		page := mustSnapshot(t, s, api.SnapshotRequest{Limit: 1, Cursor: cursor})
		if page.At.IsZero() {
			t.Error("a page of the walk has no At")
		}
		walked = append(walked, snapshotIDs(page)...)
		if page.NextCursor == "" {
			if page.Truncated {
				t.Errorf("last page truncated=%v with no cursor", page.Truncated)
			}
			break
		}
		if !page.Truncated {
			t.Errorf("page carries cursor %q but does not say it is cut", page.NextCursor)
		}
		if pages > 10 {
			t.Fatal("paging does not end")
		}
		cursor = page.NextCursor
	}
	sameSet(t, "the walk over every page", walked, blocker.ID, blocked.ID, third.ID, hq.ID)

	if _, err := s.TownSnapshot(ctx(), api.SnapshotRequest{Cursor: "not-a-cursor"}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("TownSnapshot with a bad cursor = %v, want api.ErrInvalid", err)
	}
}

package landworker

import (
	"encoding/json"
	"testing"
	"time"
)

var orphanT0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func TestPostLandOrphanEndedMarks(t *testing.T) {
	t.Parallel()
	var e PendingPostLand
	end := RunEnd{Run: 11205, Status: "cancelled", Ended: true}

	if !e.markOrphan(orphanT0, end) {
		t.Fatal("an ended answer did not mark the entry")
	}
	if !e.OrphanedAt.Equal(orphanT0) {
		t.Errorf("OrphanedAt = %v, want %v", e.OrphanedAt, orphanT0)
	}
	if got, want := e.OrphanRun, "11205 cancelled"; got != want {
		t.Errorf("OrphanRun = %q, want %q", got, want)
	}
}

func TestPostLandOrphanSecondEndedKeepsFirst(t *testing.T) {
	t.Parallel()
	var e PendingPostLand
	e.markOrphan(orphanT0, RunEnd{Run: 11205, Status: "cancelled", Ended: true})
	later := orphanT0.Add(time.Minute)

	if e.markOrphan(later, RunEnd{Run: 11206, Status: "failure", Ended: true}) {
		t.Fatal("a second ended answer marked the entry again")
	}
	if !e.OrphanedAt.Equal(orphanT0) {
		t.Errorf("OrphanedAt = %v, want the first mark %v", e.OrphanedAt, orphanT0)
	}
	if got, want := e.OrphanRun, "11205 cancelled"; got != want {
		t.Errorf("OrphanRun = %q, want the first run %q", got, want)
	}
}

func TestPostLandOrphanNotEndedDoesNotMark(t *testing.T) {
	t.Parallel()
	var e PendingPostLand

	if e.markOrphan(orphanT0, RunEnd{Run: 11205, Status: "running"}) {
		t.Fatal("a run still reporting marked the entry")
	}
	if !e.OrphanedAt.IsZero() || e.OrphanRun != "" {
		t.Errorf("an unended answer marked the entry: OrphanedAt %v, OrphanRun %q", e.OrphanedAt, e.OrphanRun)
	}
}

func TestPostLandOrphanFieldsSurviveJSON(t *testing.T) {
	t.Parallel()
	e := PendingPostLand{
		Land:       PostLand{BeadID: "gt-c2es4c", Commit: "abc1234"},
		Context:    "ci / post-land (push)",
		Since:      orphanT0,
		OrphanedAt: orphanT0.Add(10 * time.Minute),
		OrphanRun:  "11205 cancelled",
	}

	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got PendingPostLand
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OrphanedAt.Equal(e.OrphanedAt) {
		t.Errorf("OrphanedAt = %v after a round trip, want %v", got.OrphanedAt, e.OrphanedAt)
	}
	if got.OrphanRun != e.OrphanRun {
		t.Errorf("OrphanRun = %q after a round trip, want %q", got.OrphanRun, e.OrphanRun)
	}
}

package usageattrib

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// replayBase is the End of the first record a replay test writes.
var replayBase = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

// replayRec is a record for bead, kind and workspace, ending at end.
func replayRec(bead, kind, ws string, end time.Time) dashboard.UsageRecord {
	return dashboard.UsageRecord{Bead: bead, Kind: kind, Class: "polecat", Rig: "gastown", Workspace: ws, Start: end.Add(-time.Minute), End: end}
}

// putFile writes rec as the record file at path, without the writer's checks.
func putFile(t *testing.T, path string, rec dashboard.UsageRecord) {
	t.Helper()
	data, err := json.Marshal(storedRecord{UsageRecord: rec, Sessions: []string{"s-" + rec.Bead}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readRows(t *testing.T, store *beadsfake.Fake) []beads.UsageRecordRow {
	t.Helper()
	rows, err := store.ReadUsageRecords(context.Background(), time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestReplayWritesTheRowsOfFilesWithNoneOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := map[[2]string]time.Time{}
	for i, k := range [][2]string{{"gt-a", dashboard.UsageKindWork}, {"gt-a", dashboard.UsageKindOM}, {"gt-b", dashboard.UsageKindWork}} {
		end := replayBase.Add(time.Duration(i) * time.Minute)
		putFile(t, recordPath(dir, k[0], k[1]), replayRec(k[0], k[1], "ws-a", end))
		want[k] = end
	}
	store := beadsfake.New()
	res, err := Replay(context.Background(), dir, store, false)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if res.Files != 3 || res.Written != 3 || res.Skipped != 0 || len(res.Failed) != 0 {
		t.Fatalf("Replay = %+v, want 3 files, 3 written", res)
	}
	rows := readRows(t, store)
	if len(rows) != 3 {
		t.Fatalf("store holds %d rows, want 3", len(rows))
	}
	for _, row := range rows {
		if end := want[[2]string{row.Record.Bead, row.Record.Kind}]; !row.BuiltAt.Equal(end) {
			t.Errorf("%s/%s row BuiltAt = %v, want its End %v", row.Record.Bead, row.Record.Kind, row.BuiltAt, end)
		}
	}
	again, err := Replay(context.Background(), dir, store, false)
	if err != nil {
		t.Fatalf("second Replay: %v", err)
	}
	rows = readRows(t, store)
	if again.Written != 0 || again.Skipped != 3 || len(again.Failed) != 0 || len(rows) != 3 {
		t.Errorf("second Replay = %+v over %d rows, want none written, 3 skipped, 3 rows", again, len(rows))
	}
}

func TestReplaySkipsExistingRowsAndADryRunCountsWhatAnApplyWould(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	putFile(t, recordPath(dir, "gt-a", dashboard.UsageKindWork), replayRec("gt-a", dashboard.UsageKindWork, "ws-a", replayBase))
	putFile(t, recordPath(dir, "gt-a", dashboard.UsageKindOM), replayRec("gt-a", dashboard.UsageKindOM, "ws-a", replayBase.Add(time.Minute)))
	putFile(t, recordPath(dir, "gt-b", dashboard.UsageKindWork), replayRec("gt-b", dashboard.UsageKindWork, "ws-a", replayBase.Add(2*time.Minute)))
	putFile(t, recordPath(dir, "gt-zero", dashboard.UsageKindWork), replayRec("gt-zero", dashboard.UsageKindWork, "ws-a", time.Time{}))
	store, other := beadsfake.New(), beadsfake.New()
	seed := replayRec("gt-a", dashboard.UsageKindWork, "ws-a", replayBase.Add(-time.Hour))
	if err := store.WriteUsageRecord(context.Background(), beads.UsageRecordRow{BuiltAt: seed.End, Record: seed}); err != nil {
		t.Fatal(err)
	}
	// The other store's gt-b work row must not count: the check reads the store it writes to.
	foreign := replayRec("gt-b", dashboard.UsageKindWork, "ws-a", seed.End)
	if err := other.WriteUsageRecord(context.Background(), beads.UsageRecordRow{BuiltAt: foreign.End, Record: foreign}); err != nil {
		t.Fatal(err)
	}
	dry, err := Replay(context.Background(), dir, store, true)
	if err != nil {
		t.Fatalf("dry Replay: %v", err)
	}
	if rows := readRows(t, store); len(rows) != 1 {
		t.Fatalf("the dry run wrote a row: store holds %d", len(rows))
	}
	apply, err := Replay(context.Background(), dir, store, false)
	if err != nil {
		t.Fatalf("apply Replay: %v", err)
	}
	if dry.Files != apply.Files || dry.Written != apply.Written || dry.Skipped != apply.Skipped || len(dry.Failed) != len(apply.Failed) {
		t.Errorf("dry run = %+v, apply = %+v; want the same counts", dry, apply)
	}
	if apply.Written != 2 || apply.Skipped != 1 || len(apply.Failed) != 1 {
		t.Fatalf("apply = %+v, want 2 written, 1 skipped, 1 failed", apply)
	}
	if rows := readRows(t, store); len(rows) != 3 {
		t.Fatalf("store holds %d rows, want the seed and the 2 written", len(rows))
	}
}

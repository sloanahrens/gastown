package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// eventsFailureNow is the fake clock these tests start at; it is inside the
// day a full export is allowed to wait for (eventsFullExportEvery).
var eventsFailureNow = time.Date(2026, 10, 31, 10, 5, 0, 0, time.UTC)

// seedEventsExport writes the events export and the state that matches it, so
// the next read sees a file it can append to.
func seedEventsExport(t *testing.T, dir string, rows ...eventsTestRow) {
	t.Helper()
	var lines []string
	for _, row := range rows {
		line, err := json.Marshal(map[string]string{"id": row.id, "created_at": row.createdAt})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(line))
	}
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	state := eventsExportState{
		Version:        eventsExportStateVersion,
		Rows:           len(rows),
		FullExportedAt: eventsFailureNow,
	}
	if len(rows) > 0 {
		newest := rows[len(rows)-1]
		state.LastCreatedAt = newest.createdAt
		for _, row := range rows {
			if row.createdAt == newest.createdAt {
				state.BoundaryIDs = append(state.BoundaryIDs, row.id)
			}
		}
	}
	if err := writeEventsExportState(dir, state); err != nil {
		t.Fatal(err)
	}
}

// eventsExportSnapshot is what the events export in dir holds: each shard's
// bytes and the state recorded beside it, so a test can show a failed append
// changed neither.
type eventsExportSnapshot struct {
	shards map[string]string
	state  string
}

// snapshotEventsExport reads dir's shards and its recorded state.
func snapshotEventsExport(t *testing.T, dir string) eventsExportSnapshot {
	t.Helper()
	paths, err := jsonlShardPaths(dir, "events")
	if err != nil {
		t.Fatalf("listing the events export: %v", err)
	}
	snapshot := eventsExportSnapshot{shards: make(map[string]string, len(paths))}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", filepath.Base(path), err)
		}
		snapshot.shards[filepath.Base(path)] = string(b)
	}
	state, err := os.ReadFile(eventsExportStatePath(dir))
	if err != nil {
		t.Fatalf("reading the state: %v", err)
	}
	snapshot.state = string(state)
	return snapshot
}

// assertHeld fails when dir no longer holds the export the snapshot recorded,
// shard for shard and state byte for byte.
func (s eventsExportSnapshot) assertHeld(t *testing.T, dir string) {
	t.Helper()
	now := snapshotEventsExport(t, dir)
	if len(now.shards) != len(s.shards) {
		t.Errorf("the failed append changed the shard count: %d now, want the %d it started with", len(now.shards), len(s.shards))
	}
	for name, want := range s.shards {
		if got, ok := now.shards[name]; !ok {
			t.Errorf("the failed append removed %s", name)
		} else if got != want {
			t.Errorf("the failed append rewrote %s:\n got %s\nwant %s", name, got, want)
		}
	}
	if now.state != s.state {
		t.Errorf("the failed append changed the recorded state:\n got %s\nwant %s", now.state, s.state)
	}
}

// writeEventsShard writes one shard of an events export.
func writeEventsShard(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatal(err)
	}
}

// eventsLine is one exported events row.
func eventsLine(id, at string) string {
	return `{"id":"` + id + `","created_at":"` + at + `"}`
}

// stagedEventsExport is an exporter that writes lines as the staged read. With
// no lines it writes nothing at all, the way an exporter that failed to write
// its file leaves the staging dir empty.
func stagedEventsExport(lines ...string) jsonlTableExporter {
	return func(_ string, _ beadsql.Query, dir, _ string, _ time.Duration) (int, error) {
		if len(lines) == 0 {
			return 0, nil
		}
		body := strings.Join(lines, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(body), 0644); err != nil {
			return 0, err
		}
		return len(lines), nil
	}
}

// failingEventsExport is an exporter whose read fails.
func failingEventsExport(err error) jsonlTableExporter {
	return func(string, beadsql.Query, string, string, time.Duration) (int, error) { return 0, err }
}

// The failing ops below are the export's real filesystem work with one call
// replaced by an error. A mode that refuses a write to a normal user still
// accepts it from a process running as root, so the failure branches are
// driven by an injected error instead (gt-l7eeb).

// eventsOpsWithWriteFile is osEventsExportOps with every write failing.
func eventsOpsWithWriteFile(err error) eventsExportOps {
	ops := osEventsExportOps()
	ops.writeFile = func(string, []byte, os.FileMode) error { return err }
	return ops
}

// eventsOpsWithRename is osEventsExportOps with every rename failing.
func eventsOpsWithRename(err error) eventsExportOps {
	ops := osEventsExportOps()
	ops.rename = func(string, string) error { return err }
	return ops
}

// eventsOpsWithRemoveAll is osEventsExportOps with every recursive removal
// failing.
func eventsOpsWithRemoveAll(err error) eventsExportOps {
	ops := osEventsExportOps()
	ops.removeAll = func(string) error { return err }
	return ops
}

// eventsOpsWithMkdirAll is osEventsExportOps with every directory creation
// failing.
func eventsOpsWithMkdirAll(err error) eventsExportOps {
	ops := osEventsExportOps()
	ops.mkdirAll = func(string, os.FileMode) error { return err }
	return ops
}

func TestEventsExportStateRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := eventsExportState{
		Version:        eventsExportStateVersion,
		LastCreatedAt:  "2026-10-31 10:00:00",
		BoundaryIDs:    []string{"e1", "e2"},
		Rows:           2,
		FullExportedAt: eventsFailureNow,
	}
	if err := writeEventsExportState(dir, want); err != nil {
		t.Fatalf("writeEventsExportState: %v", err)
	}
	got, err := readEventsExportState(dir)
	if err != nil {
		t.Fatalf("readEventsExportState: %v", err)
	}
	if got.Rows != 2 || got.LastCreatedAt != want.LastCreatedAt || len(got.BoundaryIDs) != 2 {
		t.Errorf("state = %+v, want %+v", *got, want)
	}

	if _, err := readEventsExportState(t.TempDir()); err == nil {
		t.Error("a directory with no state file was read as a state")
	}
}

func TestReadEventsExportStateRefusesAnUntrustworthyState(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		body string
	}{
		{"not json", "nope\n"},
		{"another version", `{"version":2,"rows":0,"full_exported_at":"2026-10-31T10:05:00Z"}`},
		{"negative rows", `{"version":1,"rows":-1,"full_exported_at":"2026-10-31T10:05:00Z"}`},
		{"no full export time", `{"version":1,"rows":0}`},
		{"rows without a boundary", `{"version":1,"rows":2,"full_exported_at":"2026-10-31T10:05:00Z"}`},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(eventsExportStatePath(dir), []byte(tt.body), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := readEventsExportState(dir); err == nil {
			t.Errorf("%s: the state was trusted", tt.name)
		}
	}
}

func TestWriteEventsExportStateReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	state := eventsExportState{Version: eventsExportStateVersion, FullExportedAt: eventsFailureNow}

	writeFails := t.TempDir()
	if err := os.MkdirAll(eventsExportStatePath(writeFails)+".tmp", 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeEventsExportState(writeFails, state); err == nil {
		t.Error("a temp path it cannot write to still recorded the state")
	}

	renameFails := t.TempDir()
	if err := os.MkdirAll(eventsExportStatePath(renameFails), 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeEventsExportState(renameFails, state); err == nil {
		t.Error("a state path it cannot replace still recorded the state")
	}
}

// TestEventsExportNeedsFullWhenTheExportCannotBeRead: an export the cycle
// cannot list or count is rebuilt from the whole table, never appended to from
// a boundary whose file it could not check.
func TestEventsExportNeedsFullWhenTheExportCannotBeRead(t *testing.T) {
	t.Parallel()
	state := &eventsExportState{Version: eventsExportStateVersion, Rows: 0, FullExportedAt: eventsFailureNow}
	if full, why := eventsExportNeedsFull(filepath.Join(t.TempDir(), "missing"), state, nil, eventsFailureNow); !full {
		t.Errorf("an export that cannot be listed was appended to (%s)", why)
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "events.jsonl"), 0755); err != nil {
		t.Fatal(err)
	}
	if full, why := eventsExportNeedsFull(dir, state, nil, eventsFailureNow); !full {
		t.Errorf("an export whose shard cannot be counted was appended to (%s)", why)
	}
}

// TestClearEventsScratchReportsAFailure: a scratch it cannot clear is reported,
// so the commit never carries a staged read or a temp shard.
func TestClearEventsScratchReportsAFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stage := filepath.Join(dir, eventsIncomingDir)
	if err := os.MkdirAll(stage, 0755); err != nil {
		t.Fatal(err)
	}
	writeEventsShard(t, stage, "events.jsonl", eventsLine("e1", "2026-10-31 10:00:00"))
	ops := eventsOpsWithRemoveAll(errors.New("the staging dir is busy"))
	if err := clearEventsScratchWith(ops, dir); err == nil {
		t.Error("a staging dir it cannot remove was cleared silently")
	}

	// The export reports it too, rather than append to a file beside a staged
	// read the commit would carry.
	d, _ := backupTestDaemon(t)
	d.clock = clockwork.NewFakeClockAt(eventsFailureNow)
	other := t.TempDir()
	otherStage := filepath.Join(other, eventsIncomingDir)
	if err := os.MkdirAll(otherStage, 0755); err != nil {
		t.Fatal(err)
	}
	writeEventsShard(t, otherStage, "events.jsonl", eventsLine("e1", "2026-10-31 10:00:00"))
	if _, err := d.exportEventsToJsonlWith(ops, "hq", other, t.TempDir(), time.Minute, stagedEventsExport()); err == nil {
		t.Error("an export whose scratch could not be cleared appended to it")
	}
}

// TestAppendNewEventsReportsItsFailures drives every way the incremental read
// fails: each one is an error the caller answers by reading the whole table.
func TestAppendNewEventsReportsItsFailures(t *testing.T) {
	t.Parallel()
	newRow := eventsLine("e2", "2026-10-31 10:02:00")
	for _, tt := range []struct {
		name   string
		ops    eventsExportOps
		export jsonlTableExporter
	}{
		{
			name:   "staged read cannot be created",
			ops:    eventsOpsWithMkdirAll(errors.New("the export dir is full")),
			export: stagedEventsExport(newRow),
		},
		{
			name:   "the dolt read fails",
			export: failingEventsExport(errors.New("signal: killed")),
		},
		{
			name:   "the staged read is missing",
			export: stagedEventsExport(),
		},
		{
			name:   "a row predates the boundary",
			export: stagedEventsExport(eventsLine("e0", "2026-10-31 09:00:00")),
		},
		{
			name:   "a row cannot be placed",
			export: stagedEventsExport(`{"id":"e9"}`),
		},
		{
			name:   "the boundary cannot be recorded",
			ops:    eventsOpsWithRename(errors.New("the state cannot be replaced")),
			export: stagedEventsExport(newRow),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _ := backupTestDaemon(t)
			d.clock = clockwork.NewFakeClockAt(eventsFailureNow)
			dir := t.TempDir()
			seedEventsExport(t, dir, eventsTestRow{"e1", "2026-10-31 10:00:00"})
			state, err := readEventsExportState(dir)
			if err != nil {
				t.Fatalf("reading the seeded state: %v", err)
			}
			ops := tt.ops
			if ops.writeFile == nil {
				// The zero value is the real filesystem work.
				ops = osEventsExportOps()
			}
			if _, err := d.appendNewEventsWith(ops, "hq", dir, t.TempDir(), time.Minute, tt.export, state); err == nil {
				t.Errorf("%s: the read reported no failure", tt.name)
			}
		})
	}
}

// TestExportEventsSkipsTheFallbackWhenTheIncrementalReadSpendsItsBudget: a
// read that used the whole budget leaves nothing for the whole-table retry, so
// the cycle fails with the export and its state as they were rather than start
// a read it cannot finish (gt-l7eeb).
func TestExportEventsSkipsTheFallbackWhenTheIncrementalReadSpendsItsBudget(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	clk := clockwork.NewFakeClockAt(eventsFailureNow)
	d.clock = clk
	dir := t.TempDir()
	seedEventsExport(t, dir, eventsTestRow{"e1", "2026-10-31 10:00:00"})
	before := snapshotEventsExport(t, dir)
	budget := 2 * time.Minute
	spends := func(string, beadsql.Query, string, string, time.Duration) (int, error) {
		clk.Advance(budget)
		return 0, errors.New("the read was killed at its bound")
	}

	_, err := d.exportEventsToJsonl("hq", dir, t.TempDir(), budget, spends)
	if err == nil {
		t.Fatal("an export whose budget was spent reported success")
	}
	if !strings.Contains(err.Error(), "full fallback skipped this cycle") {
		t.Errorf("error = %v, want the spent-budget fallback named", err)
	}
	before.assertHeld(t, dir)
}

// TestExportAllEventsReportsItsFailures: a whole-table read that fails is an
// error, and so is one whose export is on disk but whose boundary cannot be
// recorded.
func TestExportAllEventsReportsItsFailures(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	d.clock = clockwork.NewFakeClockAt(eventsFailureNow)
	if _, err := d.exportAllEventsToJsonl("hq", t.TempDir(), t.TempDir(), time.Minute, failingEventsExport(errors.New("signal: killed"))); err == nil {
		t.Error("a failed whole-table read was reported as an export")
	}

	ops := eventsOpsWithRename(errors.New("the state cannot be replaced"))
	if _, err := d.exportAllEventsToJsonlWith(ops, "hq", t.TempDir(), t.TempDir(), time.Minute, stagedEventsExport(eventsLine("e1", "2026-10-31 10:00:00"))); err == nil {
		t.Error("an export whose boundary could not be recorded was reported as done")
	}
}

func TestReadStagedEventsReportsItsFailures(t *testing.T) {
	t.Parallel()
	if _, err := readEventLines(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a file that does not exist was read as empty")
	}
	if _, err := readStagedEvents(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a staging dir that cannot be listed was read as empty")
	}
	if _, err := readStagedEvents(t.TempDir()); err == nil {
		t.Error("a staging dir with no staged read was read as empty")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "events.jsonl"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := readStagedEvents(dir); err == nil {
		t.Error("a staged read that cannot be opened was read as empty")
	}
}

func TestParseEventRowRefusesARowItCannotPlace(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, line string }{
		{"not json", "nope"},
		{"no id", `{"created_at":"2026-10-31 10:00:00"}`},
		{"no created_at", `{"id":"e1"}`},
		{"created_at is not a timestamp", `{"id":"e1","created_at":"yesterday"}`},
	} {
		if _, err := parseEventRow(tt.line); err == nil {
			t.Errorf("%s: the row was placed", tt.name)
		}
	}
}

func TestAppendEventRowsReportsItsFailures(t *testing.T) {
	t.Parallel()
	newRow := json.RawMessage(eventsLine("e2", "2026-10-31 10:02:00"))
	state := func(rows int) *eventsExportState {
		return &eventsExportState{
			Version:        eventsExportStateVersion,
			LastCreatedAt:  "2026-10-31 10:00:00",
			BoundaryIDs:    []string{"e1"},
			Rows:           rows,
			FullExportedAt: eventsFailureNow,
		}
	}
	t.Run("export cannot be listed", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "missing")
		if _, err := appendEventRows(dir, []json.RawMessage{newRow}, state(1), jsonlShardMaxBytes, jsonlShardHardLimit); err == nil {
			t.Error("an export that cannot be listed was appended to")
		}
	})
	t.Run("export has no shards", func(t *testing.T) {
		t.Parallel()
		if _, err := appendEventRows(t.TempDir(), []json.RawMessage{newRow}, state(1), jsonlShardMaxBytes, jsonlShardHardLimit); err == nil {
			t.Error("an export with no shards was appended to")
		}
	})
	t.Run("a row cannot be placed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		seedEventsExport(t, dir, eventsTestRow{"e1", "2026-10-31 10:00:00"})
		if _, err := appendEventRows(dir, []json.RawMessage{json.RawMessage(`{"id":"e2"}`)}, state(1), jsonlShardMaxBytes, jsonlShardHardLimit); err == nil {
			t.Error("a row without created_at was appended")
		}
	})
	t.Run("the last shard cannot be read", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "events.jsonl"), 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := appendEventRows(dir, []json.RawMessage{newRow}, state(1), jsonlShardMaxBytes, jsonlShardHardLimit); err == nil {
			t.Error("an export whose last shard cannot be read was appended to")
		}
	})
	t.Run("a line in the export is not a row", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeEventsShard(t, dir, "events.jsonl", "nope")
		if _, err := appendEventRows(dir, []json.RawMessage{newRow}, state(1), jsonlShardMaxBytes, jsonlShardHardLimit); err == nil {
			t.Error("an export holding a line that is not a row was appended to")
		}
	})
	t.Run("a shard would be too large", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		seedEventsExport(t, dir, eventsTestRow{"e1", "2026-10-31 10:00:00"})
		before := snapshotEventsExport(t, dir)
		st := state(1)
		if _, err := appendEventRows(dir, []json.RawMessage{newRow}, st, jsonlShardMaxBytes, 1); err == nil {
			t.Error("a shard over the hard limit was written")
		}
		before.assertHeld(t, dir)
		if st.Rows != 1 || st.LastCreatedAt != "2026-10-31 10:00:00" || !slices.Equal(st.BoundaryIDs, []string{"e1"}) {
			t.Errorf("the refused append advanced the state to %+v, want the boundary it started from", st)
		}
	})
	t.Run("a shard cannot be written", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		seedEventsExport(t, dir, eventsTestRow{"e1", "2026-10-31 10:00:00"})
		before := snapshotEventsExport(t, dir)
		st := state(1)
		ops := eventsOpsWithWriteFile(errors.New("the disk is full"))
		if _, err := appendEventRowsWith(ops, dir, []json.RawMessage{newRow}, st, jsonlShardMaxBytes, jsonlShardHardLimit); err == nil {
			t.Error("an append that could not write the shard reported success")
		}
		before.assertHeld(t, dir)
		if st.Rows != 1 || st.LastCreatedAt != "2026-10-31 10:00:00" || !slices.Equal(st.BoundaryIDs, []string{"e1"}) {
			t.Errorf("the failed append advanced the state to %+v, want the boundary it started from", st)
		}
	})
	t.Run("the appended file fails its count", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		seedEventsExport(t, dir, eventsTestRow{"e1", "2026-10-31 10:00:00"})
		if _, err := appendEventRows(dir, []json.RawMessage{newRow}, state(99), jsonlShardMaxBytes, jsonlShardHardLimit); err == nil {
			t.Error("an export whose rows disagree with the state was accepted")
		}
	})
}

func TestEventsExportBoundaryReportsItsFailures(t *testing.T) {
	t.Parallel()
	at := "2026-10-31 10:00:00"
	t.Run("export cannot be listed", func(t *testing.T) {
		t.Parallel()
		if _, _, err := eventsExportBoundary(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Error("a directory that cannot be listed was read as a boundary")
		}
	})
	t.Run("export has no shards", func(t *testing.T) {
		t.Parallel()
		if _, _, err := eventsExportBoundary(t.TempDir()); err == nil {
			t.Error("an export with no shards was read as a boundary")
		}
	})
	t.Run("the last shard cannot be read", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "events.jsonl"), 0755); err != nil {
			t.Fatal(err)
		}
		if _, _, err := eventsExportBoundary(dir); err == nil {
			t.Error("a last shard that cannot be read was read as a boundary")
		}
	})
	t.Run("the export is empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeEventsShard(t, dir, "events.jsonl")
		if _, _, err := eventsExportBoundary(dir); err == nil {
			t.Error("an empty export was read as a boundary")
		}
	})
	t.Run("the newest line is not a row", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeEventsShard(t, dir, "events.jsonl", "nope")
		if _, _, err := eventsExportBoundary(dir); err == nil {
			t.Error("a line that is not a row was read as the newest row")
		}
	})
	t.Run("an earlier line is not a row", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeEventsShard(t, dir, "events.jsonl", "nope", eventsLine("e1", at))
		if _, _, err := eventsExportBoundary(dir); err == nil {
			t.Error("a line that is not a row was read as part of the boundary")
		}
	})
	t.Run("an earlier shard cannot be read", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "events.jsonl"), 0755); err != nil {
			t.Fatal(err)
		}
		writeEventsShard(t, dir, "events.2.jsonl", eventsLine("e2", at))
		if _, _, err := eventsExportBoundary(dir); err == nil {
			t.Error("an earlier shard that cannot be read was skipped over")
		}
	})
	t.Run("an earlier shard ends with a line that is not a row", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeEventsShard(t, dir, "events.jsonl", "nope")
		writeEventsShard(t, dir, "events.2.jsonl", eventsLine("e2", at))
		if _, _, err := eventsExportBoundary(dir); err == nil {
			t.Error("an earlier shard whose last line is not a row was read as part of the boundary")
		}
	})
}

func TestPublishEventShardsReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	shards := newShardBuilder(1024)
	shards.add([]byte("{}\n"))
	ops := eventsOpsWithWriteFile(errors.New("the disk is full"))
	if err := publishEventShardsWith(ops, t.TempDir(), 1, shards.shards()); err == nil {
		t.Error("a shard that could not be written was reported as published")
	}
}

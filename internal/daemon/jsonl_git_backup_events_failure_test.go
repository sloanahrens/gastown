package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// readOnlyDir makes dir refuse writes and restores the bit, so a test can drive
// the branch a failed write takes.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	//testpolicy:allow no-exec-files — the fixture directory: a read-only dir needs its search bit
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — restores the write bit so t.TempDir can clean up
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// readOnlyFile makes a file that exists refuse a write, which is how a shard
// already in the export fails to be rewritten.
func readOnlyFile(t *testing.T, path string) {
	t.Helper()
	//testpolicy:allow no-exec-files — the fixture file: a read-only file needs no search bit, and the mode carries none
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — restores the write bit so t.TempDir can clean up
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
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
	readOnlyDir(t, dir)
	if err := clearEventsScratch(dir); err == nil {
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
	readOnlyDir(t, other)
	if _, err := d.exportEventsToJsonl("hq", other, t.TempDir(), time.Minute, stagedEventsExport()); err == nil {
		t.Error("an export whose scratch could not be cleared appended to it")
	}
}

// TestAppendNewEventsReportsItsFailures drives every way the incremental read
// fails: each one is an error the caller answers by reading the whole table.
func TestAppendNewEventsReportsItsFailures(t *testing.T) {
	t.Parallel()
	newRow := eventsLine("e2", "2026-10-31 10:02:00")
	for _, tt := range []struct {
		name string
		// setup runs after the export is seeded, with the state in hand.
		setup   func(t *testing.T, dir string)
		export  jsonlTableExporter
		wantErr string
	}{
		{
			name:   "staged read cannot be created",
			setup:  func(t *testing.T, dir string) { readOnlyDir(t, dir) },
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
			name: "the boundary cannot be recorded",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(eventsExportStatePath(dir)+".tmp", 0755); err != nil {
					t.Fatal(err)
				}
			},
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
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			if _, err := d.appendNewEvents("hq", dir, t.TempDir(), time.Minute, tt.export, state); err == nil {
				t.Errorf("%s: the read reported no failure", tt.name)
			}
		})
	}
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

	dir := t.TempDir()
	if err := os.MkdirAll(eventsExportStatePath(dir)+".tmp", 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.exportAllEventsToJsonl("hq", dir, t.TempDir(), time.Minute, stagedEventsExport(eventsLine("e1", "2026-10-31 10:00:00"))); err == nil {
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
		if _, err := appendEventRows(dir, []json.RawMessage{newRow}, state(1), jsonlShardMaxBytes, 1); err == nil {
			t.Error("a shard over the hard limit was written")
		}
	})
	t.Run("a shard cannot be written", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		seedEventsExport(t, dir, eventsTestRow{"e1", "2026-10-31 10:00:00"})
		readOnlyFile(t, filepath.Join(dir, "events.jsonl"))
		if _, err := appendEventRows(dir, []json.RawMessage{newRow}, state(1), jsonlShardMaxBytes, jsonlShardHardLimit); err == nil {
			t.Error("an append that could not write the shard reported success")
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
	dir := t.TempDir()
	readOnlyDir(t, dir)
	shards := newShardBuilder(1024)
	shards.add([]byte("{}\n"))
	if err := publishEventShards(dir, 1, shards.shards()); err == nil {
		t.Error("a shard that could not be written was reported as published")
	}
}

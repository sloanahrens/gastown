package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
)

const (
	// eventsExportStateVersion is the state shape this code reads. A state of
	// another version is one it cannot trust, so the export it describes is
	// rebuilt from the whole table.
	eventsExportStateVersion = 1

	// eventsExportStateName is the boundary file kept beside the events
	// export. It is committed with the export, so a daemon restart resumes
	// from the same boundary the file ends at.
	eventsExportStateName = "events.export-state.json"

	// eventsIncomingDir is where an incremental read is written before its
	// rows are appended. The table exporter writes a file, and the append
	// needs the rows back from it (gt-l7eeb).
	eventsIncomingDir = ".events-incoming"

	// eventsFullExportEvery bounds how long the export may go without being
	// rebuilt from the whole table: a deleted issue takes its events with it
	// through the foreign-key cascade, and an append-only file would keep them
	// for ever (gt-l7eeb).
	eventsFullExportEvery = 24 * time.Hour
)

// errEventsBoundarySpansShards marks an append that cannot keep the file in
// order: the rows sharing the boundary timestamp fill the last shard and run
// on into the one before it, so their merge point is not in one file.
var errEventsBoundarySpansShards = errors.New("the boundary rows span shards")

// eventsExportOps is the filesystem work an events export does. Production
// passes the os package; a test that drives a failing write passes its own,
// since a mode that refuses a write to a normal user still accepts it when the
// tests run as root (gt-l7eeb).
type eventsExportOps struct {
	writeFile func(name string, data []byte, perm os.FileMode) error
	rename    func(oldpath, newpath string) error
	remove    func(path string) error
	removeAll func(path string) error
	mkdirAll  func(path string, perm os.FileMode) error
}

// osEventsExportOps is the export's real filesystem work.
func osEventsExportOps() eventsExportOps {
	return eventsExportOps{
		writeFile: os.WriteFile,
		rename:    os.Rename,
		remove:    os.Remove,
		removeAll: os.RemoveAll,
		mkdirAll:  os.MkdirAll,
	}
}

// eventsExportState is what the next events export appends from: the newest
// created_at already in the file, the ids that share it (a re-read of that
// second must not add a row twice), the rows the file holds, and when the
// export was last rebuilt from the whole table.
type eventsExportState struct {
	Version        int       `json:"version"`
	LastCreatedAt  string    `json:"last_created_at"`
	BoundaryIDs    []string  `json:"boundary_ids,omitempty"`
	Rows           int       `json:"rows"`
	FullExportedAt time.Time `json:"full_exported_at"`
}

// eventsExportStatePath is where dir's events export records its boundary.
func eventsExportStatePath(dir string) string {
	return filepath.Join(dir, eventsExportStateName)
}

// readEventsExportState reads dir's state. An error means the state cannot be
// trusted, and the export it describes is rebuilt from the whole table.
func readEventsExportState(dir string) (*eventsExportState, error) {
	data, err := os.ReadFile(eventsExportStatePath(dir))
	if err != nil {
		return nil, err
	}
	var st eventsExportState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", eventsExportStateName, err)
	}
	switch {
	case st.Version != eventsExportStateVersion:
		return nil, fmt.Errorf("%s: version %d, want %d", eventsExportStateName, st.Version, eventsExportStateVersion)
	case st.Rows < 0:
		return nil, fmt.Errorf("%s: %d rows", eventsExportStateName, st.Rows)
	case st.FullExportedAt.IsZero():
		return nil, fmt.Errorf("%s: no full export time", eventsExportStateName)
	case st.Rows > 0 && !beadsql.TimestampLiteral(st.LastCreatedAt):
		return nil, fmt.Errorf("%s: %d rows but its boundary %q is not a timestamp", eventsExportStateName, st.Rows, st.LastCreatedAt)
	}
	return &st, nil
}

// eventsExportStateJSON encodes st as the one line the state file holds. The
// type holds only values encoding/json cannot refuse, so a failure here is a
// bug in this file rather than a state it cannot write.
func eventsExportStateJSON(st eventsExportState) []byte {
	data, err := json.Marshal(st)
	if err != nil {
		panic("encoding the events export state: " + err.Error())
	}
	return append(data, '\n')
}

// writeEventsExportState replaces dir's state atomically: a cycle killed
// mid-write leaves the previous state, and the row count it records no longer
// matches the file the killed append touched, so the next export is full.
func writeEventsExportState(dir string, st eventsExportState) error {
	return writeEventsExportStateWith(osEventsExportOps(), dir, st)
}

func writeEventsExportStateWith(ops eventsExportOps, dir string, st eventsExportState) error {
	path := eventsExportStatePath(dir)
	tmp := path + ".tmp"
	if err := ops.writeFile(tmp, eventsExportStateJSON(st), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(tmp), err)
	}
	if err := ops.rename(tmp, path); err != nil {
		_ = ops.remove(tmp)
		return fmt.Errorf("publishing %s: %w", filepath.Base(path), err)
	}
	return nil
}

// eventsExportNeedsFull reports whether the events export in dir must be
// rebuilt from the whole table rather than appended to, and why. A state or
// file it cannot read is a full export, never an append from a boundary it
// does not hold: an append from the wrong boundary misses rows silently.
func eventsExportNeedsFull(dir string, st *eventsExportState, stateErr error, now time.Time) (bool, string) {
	if stateErr != nil {
		if errors.Is(stateErr, fs.ErrNotExist) {
			return true, "there is no state file"
		}
		return true, fmt.Sprintf("the state file is unreadable: %v", stateErr)
	}
	paths, err := jsonlShardPaths(dir, "events")
	if err != nil {
		return true, fmt.Sprintf("the export cannot be listed: %v", err)
	}
	if len(paths) == 0 {
		return true, "the export is missing"
	}
	rows := 0
	for _, path := range paths {
		n, err := countFileLines(path)
		if err != nil {
			return true, fmt.Sprintf("%s cannot be read: %v", filepath.Base(path), err)
		}
		rows += n
	}
	if rows != st.Rows {
		return true, fmt.Sprintf("the export holds %d rows, the state records %d", rows, st.Rows)
	}
	if age := now.Sub(st.FullExportedAt); age >= eventsFullExportEvery {
		return true, fmt.Sprintf("the last full export was %s ago", age.Round(time.Hour))
	}
	return false, ""
}

// exportEventsToJsonl exports database's events table to dir, appending the
// rows created since the boundary the last export recorded. The whole table is
// read, and the file replaced, when the append cannot be trusted: see
// eventsExportNeedsFull, and an append that fails part way.
//
// The returned count is the rows the file holds after the export, so the
// cycle's count stays comparable across a full and an incremental cycle.
//
// A row that commits after a cycle recorded a later boundary but carries an
// older created_at is not read by the incremental cycles; the next full pass,
// at most eventsFullExportEvery later, includes it.
func (d *Daemon) exportEventsToJsonl(db, dir, dataDir string, budget time.Duration, exportTable jsonlTableExporter) (int, error) {
	return d.exportEventsToJsonlWith(osEventsExportOps(), db, dir, dataDir, budget, exportTable)
}

func (d *Daemon) exportEventsToJsonlWith(ops eventsExportOps, db, dir, dataDir string, budget time.Duration, exportTable jsonlTableExporter) (int, error) {
	// A staged read and a temp shard a killed cycle left are cleared before
	// either path runs, so the commit never carries them (gt-fhcwe, gt-1911b).
	if err := clearEventsScratchWith(ops, dir); err != nil {
		return 0, err
	}

	state, stateErr := readEventsExportState(dir)
	if full, why := eventsExportNeedsFull(dir, state, stateErr, d.clk().Now()); full {
		d.logger.Printf("jsonl_git_backup: %s/events: full export — %s", db, why)
		return d.exportAllEventsToJsonlWith(ops, db, dir, dataDir, budget, exportTable)
	}

	start := d.clk().Now()
	total, err := d.appendNewEventsWith(ops, db, dir, dataDir, budget, exportTable, state)
	if err == nil {
		return total, nil
	}
	// A half-appended file, or one whose boundary rows cannot be merged in
	// order, is not a file to keep building on: reading the whole table
	// replaces it and records a fresh boundary. It gets what is left of the
	// table's budget; a spent one fails the export, leaving the files and the
	// state as they were, rather than start a read it cannot finish.
	d.logger.Printf("jsonl_git_backup: %s/events: the append failed (%v) — reading the whole table instead", db, err)
	remaining := budget - d.clk().Now().Sub(start)
	if remaining <= 0 {
		return 0, errors.New("events incremental export spent its budget; full fallback skipped this cycle")
	}
	return d.exportAllEventsToJsonlWith(ops, db, dir, dataDir, remaining, exportTable)
}

// clearEventsScratchWith removes what a killed cycle left beside the export:
// the staged read, and a temp shard whose write it did not finish.
func clearEventsScratchWith(ops eventsExportOps, dir string) error {
	err := ops.removeAll(filepath.Join(dir, eventsIncomingDir))
	if err != nil {
		err = fmt.Errorf("clearing %s: %w", eventsIncomingDir, err)
	}
	if tempErr := removeShardTemps(ops.removeAll, dir, "events"); tempErr != nil {
		err = errors.Join(err, fmt.Errorf("clearing events temp files: %w", tempErr))
	}
	return err
}

// appendNewEvents reads the events created since state's boundary and appends
// them to the export in dir.
func (d *Daemon) appendNewEvents(db, dir, dataDir string, budget time.Duration, exportTable jsonlTableExporter, state *eventsExportState) (int, error) {
	return d.appendNewEventsWith(osEventsExportOps(), db, dir, dataDir, budget, exportTable, state)
}

func (d *Daemon) appendNewEventsWith(ops eventsExportOps, db, dir, dataDir string, budget time.Duration, exportTable jsonlTableExporter, state *eventsExportState) (int, error) {
	stage := filepath.Join(dir, eventsIncomingDir)
	if err := ops.mkdirAll(stage, 0755); err != nil {
		return 0, fmt.Errorf("creating %s: %w", eventsIncomingDir, err)
	}
	defer func() { _ = ops.removeAll(stage) }()

	if _, err := exportTable("events", beadsql.BackupEvents(db, state.LastCreatedAt), stage, dataDir, budget); err != nil {
		return 0, err
	}
	rows, err := readStagedEvents(stage)
	if err != nil {
		return 0, err
	}
	if before, ok := eventsRowsBefore(rows, state.LastCreatedAt); ok {
		return 0, fmt.Errorf("a row stamped %s is older than the boundary %s", before, state.LastCreatedAt)
	}

	appended, err := appendEventRowsWith(ops, dir, rows, state, jsonlShardMaxBytes, jsonlShardHardLimit)
	if err != nil {
		return 0, err
	}
	if appended == 0 {
		return state.Rows, nil
	}
	if err := writeEventsExportStateWith(ops, dir, *state); err != nil {
		return 0, fmt.Errorf("recording the events export boundary: %w", err)
	}
	return state.Rows, nil
}

// exportAllEventsToJsonl reads the whole events table into dir, replacing the
// export, and records the boundary the new file ends at. exportTable is the
// cycle's table exporter, the seam a test drives.
func (d *Daemon) exportAllEventsToJsonl(db, dir, dataDir string, budget time.Duration, exportTable jsonlTableExporter) (int, error) {
	return d.exportAllEventsToJsonlWith(osEventsExportOps(), db, dir, dataDir, budget, exportTable)
}

func (d *Daemon) exportAllEventsToJsonlWith(ops eventsExportOps, db, dir, dataDir string, budget time.Duration, exportTable jsonlTableExporter) (int, error) {
	n, err := exportTable("events", beadsql.BackupEvents(db, ""), dir, dataDir, budget)
	if err != nil {
		return 0, err
	}
	state := eventsExportState{Version: eventsExportStateVersion, Rows: n, FullExportedAt: d.clk().Now()}
	if n > 0 {
		boundary, ids, err := eventsExportBoundary(dir)
		if err != nil {
			// The file is on disk but its newest row cannot be read back, so
			// there is no boundary to append from. The state records none, and
			// the next cycle reads the whole table again rather than append
			// from a boundary this export could not show.
			d.logger.Printf("jsonl_git_backup: %s/events: the export's newest row cannot be read back (%v) — the next cycle will read the whole table", db, err)
		} else {
			state.LastCreatedAt, state.BoundaryIDs = boundary, ids
		}
	}
	if err := writeEventsExportStateWith(ops, dir, state); err != nil {
		return 0, fmt.Errorf("recording the events export: %w", err)
	}
	return n, nil
}

// readStagedEvents reads the rows an incremental read wrote to stage.
func readStagedEvents(stage string) ([]json.RawMessage, error) {
	paths, err := jsonlShardPaths(stage, "events")
	if err != nil {
		return nil, fmt.Errorf("listing the staged events read: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("the staged events read is missing")
	}
	var rows []json.RawMessage
	for _, path := range paths {
		lines, err := readEventLines(path)
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			rows = append(rows, json.RawMessage(line))
		}
	}
	return rows, nil
}

// readEventLines reads path's non-empty lines without a trailing newline. The
// reader is uncapped: an events row may carry a value larger than the
// scanner's default line length.
func readEventLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	var lines []string
	for {
		line, readErr := reader.ReadBytes('\n')
		if line = trimLineEnding(line); len(line) > 0 {
			lines = append(lines, string(line))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return lines, nil
			}
			return nil, readErr
		}
	}
}

// eventsRow is one exported events row with the two columns the export orders
// by.
type eventsRow struct {
	line      string
	id        string
	createdAt string
}

// parseEventRow reads the id and created_at an exported events row must carry
// to be placed in the file's order.
func parseEventRow(line string) (eventsRow, error) {
	var fields struct {
		ID        *string `json:"id"`
		CreatedAt *string `json:"created_at"`
	}
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		return eventsRow{}, err
	}
	if fields.ID == nil || *fields.ID == "" {
		return eventsRow{}, errors.New("row has no id")
	}
	if fields.CreatedAt == nil || !beadsql.TimestampLiteral(*fields.CreatedAt) {
		return eventsRow{}, fmt.Errorf("row %s has no created_at timestamp", *fields.ID)
	}
	return eventsRow{line: line, id: *fields.ID, createdAt: *fields.CreatedAt}, nil
}

// appendEventRows appends rows to the events export in dir in created_at, id
// order, cutting a new shard where one would outgrow maxBytes and refusing one
// over hardLimit, updating state to the boundary the file now ends at, and
// returning the number of rows appended. Rows the state's boundary ids already
// hold are dropped, so re-reading the boundary second adds each row once.
func appendEventRows(dir string, rows []json.RawMessage, state *eventsExportState, maxBytes int, hardLimit int64) (int, error) {
	return appendEventRowsWith(osEventsExportOps(), dir, rows, state, maxBytes, hardLimit)
}

func appendEventRowsWith(ops eventsExportOps, dir string, rows []json.RawMessage, state *eventsExportState, maxBytes int, hardLimit int64) (int, error) {
	paths, err := jsonlShardPaths(dir, "events")
	if err != nil {
		return 0, fmt.Errorf("listing the events export: %w", err)
	}
	if len(paths) == 0 {
		return 0, errors.New("the events export has no shards to append to")
	}
	last := paths[len(paths)-1]
	// jsonlShardPaths lists the shards in number order, and a full export
	// numbers them from one. An export missing a shard fails the row count
	// above, so the list here is the whole file: the last path is shard
	// len(paths).
	lastNum := len(paths)

	fresh, err := freshEventRows(rows, state)
	if err != nil {
		return 0, err
	}
	if len(fresh) == 0 {
		return 0, nil
	}

	existing, err := readEventLines(last)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", filepath.Base(last), err)
	}
	// The file is ordered created_at, id, so the rows sharing the boundary
	// timestamp are the run it ends in. A new row stamped with the same time
	// belongs inside that run; appended after it, the file stops being
	// ordered.
	runStart := len(existing)
	var run []eventsRow
	for i := len(existing) - 1; i >= 0; i-- {
		row, err := parseEventRow(existing[i])
		if err != nil {
			return 0, fmt.Errorf("%s: %w", filepath.Base(last), err)
		}
		if row.createdAt != state.LastCreatedAt {
			break
		}
		run = append(run, row)
		runStart = i
	}
	if runStart == 0 && len(paths) > 1 {
		// The boundary run fills the last shard and continues into the shard
		// before it, where its ids are known to the state but its order is not
		// in this file.
		return 0, errEventsBoundarySpansShards
	}
	for i, j := 0, len(run)-1; i < j; i, j = i+1, j-1 {
		run[i], run[j] = run[j], run[i]
	}
	var atBoundary, after []eventsRow
	for _, row := range fresh {
		if row.createdAt == state.LastCreatedAt {
			atBoundary = append(atBoundary, row)
			continue
		}
		after = append(after, row)
	}
	merged := append(run, atBoundary...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].id < merged[j].id })

	b := newShardBuilder(maxBytes)
	for _, line := range existing[:runStart] {
		b.add([]byte(line + "\n"))
	}
	for _, row := range merged {
		b.add([]byte(row.line + "\n"))
	}
	for _, row := range after {
		b.add([]byte(row.line + "\n"))
	}
	shards := b.shards()
	if err := checkShardSizes("events", shards, hardLimit); err != nil {
		return 0, err
	}
	total := state.Rows + len(fresh)
	if err := publishEventShardsWith(ops, dir, lastNum, shards); err != nil {
		return 0, err
	}
	if err := verifyJsonlShards(dir, "events", total, hardLimit); err != nil {
		return 0, err
	}

	// The rows stamped at the second the file already ended on join the run it
	// ends in, so the ids the entry state held for that second stay in the
	// file. Kept only when the boundary moves on, they would be read as new by
	// the next cycle, which reads from the same second, and appended again.
	entryBoundary := state.LastCreatedAt
	entryIDs := append([]string(nil), state.BoundaryIDs...)

	state.Rows = total
	newest := fresh[len(fresh)-1]
	state.LastCreatedAt = newest.createdAt
	var boundaryIDs []string
	if newest.createdAt == entryBoundary {
		// freshEventRows dropped every row the entry ids already named, so the
		// union with the rows at that second holds each id once.
		boundaryIDs = entryIDs
	}
	for _, row := range fresh {
		if row.createdAt == newest.createdAt {
			boundaryIDs = append(boundaryIDs, row.id)
		}
	}
	state.BoundaryIDs = boundaryIDs
	return len(fresh), nil
}

// freshEventRows parses rows, drops the ones the export already holds at the
// boundary, and returns them in created_at, id order.
func freshEventRows(rows []json.RawMessage, state *eventsExportState) ([]eventsRow, error) {
	known := make(map[string]bool, len(state.BoundaryIDs))
	for _, id := range state.BoundaryIDs {
		known[id] = true
	}
	fresh := make([]eventsRow, 0, len(rows))
	for _, raw := range rows {
		row, err := parseEventRow(string(raw))
		if err != nil {
			return nil, err
		}
		if row.createdAt == state.LastCreatedAt && known[row.id] {
			continue
		}
		fresh = append(fresh, row)
	}
	sort.SliceStable(fresh, func(i, j int) bool { return eventsRowLess(fresh[i], fresh[j]) })
	return fresh, nil
}

// eventsRowsBefore names the first row stamped earlier than boundary, which an
// append at the end of the file cannot place in order.
func eventsRowsBefore(rows []json.RawMessage, boundary string) (string, bool) {
	for _, raw := range rows {
		row, err := parseEventRow(string(raw))
		if err != nil {
			continue
		}
		if compareEventTimestamps(row.createdAt, boundary) < 0 {
			return row.createdAt, true
		}
	}
	return "", false
}

// eventsRowLess orders two rows the way the export is written.
func eventsRowLess(a, b eventsRow) bool {
	if c := compareEventTimestamps(a.createdAt, b.createdAt); c != 0 {
		return c < 0
	}
	return a.id < b.id
}

// compareEventTimestamps orders two created_at values by time when both parse
// and by their text otherwise, which is the same order Dolt gives the column.
func compareEventTimestamps(a, b string) int {
	ta, okA := parseEventTimestamp(a)
	tb, okB := parseEventTimestamp(b)
	if okA && okB {
		switch {
		case ta.Before(tb):
			return -1
		case ta.After(tb):
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
}

// parseEventTimestamp reads the forms Dolt emits for created_at; the layouts
// are those internal/tracker/migrate/rows.go parses.
func parseEventTimestamp(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", time.RFC3339Nano} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// eventsExportBoundary reads the boundary a finished full export ends at: the
// newest created_at in the file and the ids that share it. The run of rows
// sharing it can continue into an earlier shard, so the ids are collected
// across shards, backwards, while each shard ends with that time.
func eventsExportBoundary(dir string) (string, []string, error) {
	paths, err := jsonlShardPaths(dir, "events")
	if err != nil {
		return "", nil, err
	}
	if len(paths) == 0 {
		return "", nil, errors.New("the events export has no shards")
	}
	last := paths[len(paths)-1]
	lines, err := readEventLines(last)
	if err != nil {
		return "", nil, err
	}
	if len(lines) == 0 {
		return "", nil, fmt.Errorf("%s is empty", filepath.Base(last))
	}
	newest, err := parseEventRow(lines[len(lines)-1])
	if err != nil {
		return "", nil, err
	}
	boundary := newest.createdAt
	var ids []string
	for i := len(paths) - 1; i >= 0; i-- {
		shardLines := lines
		if i < len(paths)-1 {
			if shardLines, err = readEventLines(paths[i]); err != nil {
				return "", nil, err
			}
		}
		if len(shardLines) == 0 {
			break
		}
		end, err := parseEventRow(shardLines[len(shardLines)-1])
		if err != nil {
			return "", nil, err
		}
		if end.createdAt != boundary {
			break
		}
		for j := len(shardLines) - 1; j >= 0; j-- {
			row, err := parseEventRow(shardLines[j])
			if err != nil {
				return "", nil, err
			}
			if row.createdAt != boundary {
				break
			}
			ids = append(ids, row.id)
		}
	}
	return boundary, ids, nil
}

// publishEventShardsWith writes the export's shards from firstNum up. A write
// that fails part way leaves a file the recorded row count no longer matches,
// which sends the next export to the whole table; nothing is left to clean up
// but the export itself, which that read replaces.
func publishEventShardsWith(ops eventsExportOps, dir string, firstNum int, shards []*bytes.Buffer) error {
	for i, shard := range shards {
		path := filepath.Join(dir, jsonlShardName("events", firstNum+i))
		if err := ops.writeFile(path, shard.Bytes(), 0644); err != nil {
			return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

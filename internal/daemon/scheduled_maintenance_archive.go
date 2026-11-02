package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/config"
)

// scheduled_maintenance's history archive (gt-7nxj4): in the maintenance
// window, and before that window's gc, each database's new dolt_log rows are
// appended to <archive_dir>/dolt-log-<db>-<UTC date>.csv.gz and its new events
// rows to <archive_dir>/events-<db>-<UTC date>.csv.gz. A flatten or rewrite
// discards dolt_log and with it every AS OF reading, so the commit file is the
// copy that survives one; the events table is in Dolt's dolt_ignore list, so no
// commit and no backup holds it at all and the live server is its only source.
// docs/dolt-history-offline.md is the flatten procedure.
//
// The reads are plain SELECTs through the running server: nothing here writes
// to Dolt. Each database's watermarks, the commit and the events row the
// archive last wrote out, live in daemon/maintenance_state.json beside the gc's
// size fields, and move only after the day's file has been flushed and closed.

const (
	// maintenanceArchiveReadTimeout bounds one archive read through the server:
	// a dolt_log query, or one events batch. A day's commits are a few thousand
	// rows; five minutes is a hang.
	maintenanceArchiveReadTimeout = 5 * time.Minute

	// maintenanceArchiveExportTimeout bounds one database's events export,
	// which for a first export is many batch reads. The commit export is one
	// read under the step's own bound and the read timeout in its DSN.
	maintenanceArchiveExportTimeout = 15 * time.Minute

	// maintenanceArchiveTimeout bounds the whole step across every database, so
	// a server that answers each read slowly cannot hold the maintenance cycle
	// (and the backup and gc behind it) indefinitely.
	maintenanceArchiveTimeout = 30 * time.Minute

	// maintenanceArchiveEventsBatch is how many events rows one read holds.
	// A first export reads the whole table, so it is read in batches the
	// daemon can hold in memory and appended batch by batch.
	maintenanceArchiveEventsBatch = 5000

	// maintenanceArchiveDirKey names the setting in log and error text.
	maintenanceArchiveDirKey = config.MaintenanceArchiveDirKey

	maintenanceArchiveSource = "scheduled_maintenance"
)

// archiveCSVHeader is the commit file's column order (gt-7nxj4).
var archiveCSVHeader = []string{"commit_hash", "committer", "date", "message"}

// ArchiveWatermark is the commit cursor a database's history archive last
// wrote out: the newest commit date written, and every commit hash written at
// that date. The next read takes commits dated at or after Date and drops the
// hashes in Hashes, so it neither repeats the boundary commits nor skips one
// that shares their date (gt-7nxj4).
//
// The cursor is a date rather than dolt_log's commit_order because the archive
// exists to survive a flatten, and a flatten replaces the history with a root
// commit whose height restarts from zero: a height cursor would then read
// every new commit as already archived. A flatten's root commit is dated after
// the old history, so it and everything after it lands above the date cursor.
// The accepted limit is a commit whose date is behind the cursor when it is
// read: a clock skewed backwards, or a history imported with older dates. The
// export does not read such a commit, but the pre-flatten check in `gt dolt
// status` does not share this cursor: it compares every hash in dolt_log with
// the hashes the archive files hold, so it counts the commit and refuses the
// flatten.
type ArchiveWatermark struct {
	Date   time.Time `json:"date"`
	Hashes []string  `json:"hashes"`
}

// Validate reports whether the watermark can bound a read: a zero watermark
// (nothing archived yet) is valid, and so is a date with the hashes written at
// it. A date with no hash cannot say which commits at that date are archived,
// so it is refused rather than read as the start of the log (which would
// export the boundary commits twice); a hash without a date cannot be placed
// in the log at all.
func (w ArchiveWatermark) Validate() error {
	if w.Date.IsZero() {
		if len(w.Hashes) > 0 {
			return fmt.Errorf("commit archive watermark lists %d hash(es) but no date; refusing to read past it", len(w.Hashes))
		}
		return nil
	}
	if len(w.Hashes) == 0 {
		return fmt.Errorf("commit archive watermark for %s lists no hashes; refusing to read past it",
			w.Date.UTC().Format(time.RFC3339))
	}
	for _, h := range w.Hashes {
		if h == "" {
			return fmt.Errorf("commit archive watermark for %s lists an empty hash; refusing to read past it",
				w.Date.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

// covers reports whether hash was already written out at the watermark's date.
func (w ArchiveWatermark) covers(hash string) bool {
	return slices.Contains(w.Hashes, hash)
}

// eventsArchiveLookback is how far before the newest events row the archive
// re-reads on the next run. events.id is a UUID, so it cannot order rows the
// way the (created_at, id) cursor used to: a row inserted with a created_at
// behind the watermark sorts behind it and would never be read. Re-reading
// the window and skipping the ids already archived picks such a row up.
//
// The one accepted limit: a row inserted more than this long after its own
// created_at stays outside the window and is never read. A backdated writer
// is the usual cause.
const eventsArchiveLookback = 24 * time.Hour

// ArchivedEventsRow is one events row the archive has written, kept so the
// lookback re-read can skip it.
type ArchivedEventsRow struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

// EventsArchiveWatermark is the events archive's position: the newest
// created_at it wrote out, and the ids of every row it wrote whose created_at
// is at or after that date minus eventsArchiveLookback. The next read starts
// at the window's start and skips these ids, so a backdated row is archived
// once and a row already written is not appended again. The zero value means
// nothing has been archived yet.
type EventsArchiveWatermark struct {
	CreatedAt string              `json:"created_at,omitempty"`
	Archived  []ArchivedEventsRow `json:"archived,omitempty"`
}

// Validate reports whether the watermark can bound a read: a zero watermark
// (nothing archived yet) is valid, and so is a timestamp with the ids written
// in its window. A timestamp with no id cannot say which rows at it are
// already archived, so it is refused rather than read as the start of the
// table (which would write those rows out twice); an id without a timestamp
// cannot be placed in the window at all (gt-7nxj4).
func (w EventsArchiveWatermark) Validate() error {
	if w.CreatedAt == "" {
		if len(w.Archived) > 0 {
			return fmt.Errorf("events archive watermark lists %d id(s) but no created_at; refusing to read past it", len(w.Archived))
		}
		return nil
	}
	if !beadsql.TimestampLiteral(w.CreatedAt) {
		return fmt.Errorf("events archive watermark created_at %q is not a timestamp", w.CreatedAt)
	}
	if len(w.Archived) == 0 {
		return fmt.Errorf("events archive watermark for %s lists no ids; refusing to read past it", w.CreatedAt)
	}
	for _, row := range w.Archived {
		if !beadsql.TimestampLiteral(row.CreatedAt) {
			return fmt.Errorf("events archive watermark for %s carries an id with created_at %q, which is not a timestamp", w.CreatedAt, row.CreatedAt)
		}
		if row.ID == "" {
			return fmt.Errorf("events archive watermark for %s carries an empty id; refusing to read past it", w.CreatedAt)
		}
	}
	return nil
}

// Since returns the first created_at the next read looks at: the newest
// written row's date minus the lookback window. An empty watermark has no
// lower bound.
func (w EventsArchiveWatermark) Since() (string, error) {
	if w.CreatedAt == "" {
		return "", nil
	}
	at, err := parseEventsTimestamp(w.CreatedAt)
	if err != nil {
		return "", err
	}
	return at.Add(-eventsArchiveLookback).UTC().Format("2006-01-02 15:04:05"), nil
}

// ArchivedIDs is the set of ids the next read must skip.
func (w EventsArchiveWatermark) ArchivedIDs() map[string]bool {
	if len(w.Archived) == 0 {
		return nil
	}
	out := make(map[string]bool, len(w.Archived))
	for _, row := range w.Archived {
		out[row.ID] = true
	}
	return out
}

// written is the watermark after rows were written: the newest created_at
// written, and every id written at or after that date minus the lookback. A
// pair from the old window whose created_at is now outside it is dropped, so
// the set stays the size of one window's rows instead of growing with the
// archive (gt-7nxj4).
func (w EventsArchiveWatermark) written(columns []string, rows [][]string) (EventsArchiveWatermark, error) {
	createdAt, id, err := eventsWatermarkColumns(columns)
	if err != nil {
		return EventsArchiveWatermark{}, err
	}
	var newest time.Time
	if w.CreatedAt != "" {
		if newest, err = parseEventsTimestamp(w.CreatedAt); err != nil {
			return EventsArchiveWatermark{}, err
		}
	}
	newestText := w.CreatedAt
	for _, row := range rows {
		if createdAt >= len(row) || id >= len(row) {
			return EventsArchiveWatermark{}, fmt.Errorf("events row of %d values is missing column %d or %d", len(row), createdAt, id)
		}
		if !beadsql.TimestampLiteral(row[createdAt]) {
			return EventsArchiveWatermark{}, fmt.Errorf("events row created_at %q is not a timestamp", row[createdAt])
		}
		if row[id] == "" {
			return EventsArchiveWatermark{}, fmt.Errorf("events row with created_at %s has no id", row[createdAt])
		}
		at, err := parseEventsTimestamp(row[createdAt])
		if err != nil {
			return EventsArchiveWatermark{}, err
		}
		if newest.IsZero() || at.After(newest) {
			newest = at
			newestText = row[createdAt]
		}
	}
	if newest.IsZero() {
		return w, nil
	}
	windowStart := newest.Add(-eventsArchiveLookback)
	set := make(map[string]ArchivedEventsRow, len(w.Archived)+len(rows))
	for _, pair := range w.Archived {
		if at, err := parseEventsTimestamp(pair.CreatedAt); err == nil && !at.Before(windowStart) {
			set[pair.ID] = pair
		}
	}
	for _, row := range rows {
		set[row[id]] = ArchivedEventsRow{CreatedAt: row[createdAt], ID: row[id]}
	}
	out := make([]ArchivedEventsRow, 0, len(set))
	for _, pair := range set {
		out = append(out, pair)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return EventsArchiveWatermark{CreatedAt: newestText, Archived: out}, nil
}

// parseEventsTimestamp parses the forms Dolt emits for a datetime column, in
// the forms beadsql.TimestampLiteral accepts.
func parseEventsTimestamp(s string) (time.Time, error) {
	layouts := []string{
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05.999999999",
	}
	for _, layout := range layouts {
		if at, err := time.Parse(layout, s); err == nil {
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("events timestamp %q cannot be parsed", s)
}

// ArchiveWatermarks is one town's history archive cursors, per database.
type ArchiveWatermarks struct {
	Commits map[string]ArchiveWatermark
	Events  map[string]EventsArchiveWatermark
}

// archiveCommit is one dolt_log row the archive writes. Message is already
// encoded for the CSV (encodeArchiveCell), so a NULL and an empty message stay
// distinguishable.
type archiveCommit struct {
	Hash      string
	Committer string
	Date      time.Time
	Message   string
}

// archiveEvents is one batch of events rows: the table's columns in query
// order, and each row's values in that order. A NULL is the archiveNullCell
// marker, distinct from an empty string (see encodeArchiveCell).
type archiveEvents struct {
	Columns []string
	Rows    [][]string
}

// archiveNullCell marks a NULL events value in the CSV. CSV has no null, so
// writing a NULL as an empty field would archive it identically to the empty
// string; encodeArchiveCell's escape is what keeps them distinguishable
// (gt-7nxj4).
const archiveNullCell = `\N`

// encodeArchiveCell renders one scanned events value for the CSV. NULL becomes
// the marker; a non-NULL value that begins with a backslash gets one more, so
// a literal `\N` reads back as data rather than as a NULL.
func encodeArchiveCell(v sql.NullString) string {
	if !v.Valid {
		return archiveNullCell
	}
	if strings.HasPrefix(v.String, `\`) {
		return `\` + v.String
	}
	return v.String
}

// maintenanceArchive appends every database's new commits and new events rows
// to the archive. One export failing keeps its watermark and is named in the
// log; the database's other export and the other databases still export.
//
// It returns the databases whose archive failed, and the cycle skips their gc:
// history is not collected for a database whose archive step did not finish.
// The gc of every other database still runs.
//
// The step reads beside a live town, but it still asks the gc's quiet-window
// guard: a busy window is a deferral (the next tick retries, as the gc's own
// deferrals do), not a failure, and the backup and gc behind it wait with it.
// A misconfigured directory is a failure and does not stop the backup or gc.
func (d *Daemon) maintenanceArchive(databases []string) (maintenanceGCResult, map[string]bool) {
	dir, err := d.maintenanceArchiveDir()
	if err != nil {
		// A set directory the daemon cannot use is a misconfiguration, not
		// "archiving is off": say so and raise it rather than archive nothing
		// silently.
		d.logger.Printf("scheduled_maintenance: archive: %v — no history archived this run", err)
		d.maintenance().escalate(d, maintenanceArchiveSource, fmt.Sprintf(
			"scheduled_maintenance: history archive: %v\nNo database's dolt_log or events was archived this run. "+
				"Fix %s in the daemon config; until then a flatten discards history it has not written out.",
			err, maintenanceArchiveDirKey))
		return maintenanceGCResult{outcome: gcOutcomeCompleted}, nil
	}
	if dir == "" {
		// Once per run, not once per database: the step is off, not broken.
		d.logger.Printf("scheduled_maintenance: archive: %s unset — history archiving is off", maintenanceArchiveDirKey)
		return maintenanceGCResult{outcome: gcOutcomeCompleted}, nil
	}
	if quiet, why := d.maintenance().quiet(d); !quiet {
		d.logger.Printf("scheduled_maintenance: archive held: %s", why)
		return maintenanceGCResult{outcome: gcOutcomeDeferred, reason: "archive: " + why}, nil
	}

	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		// Without the state file no watermark can be trusted, and rewriting it
		// would drop the real ones.
		d.logger.Printf("scheduled_maintenance: archive: %v — no history archived this run", err)
		d.maintenance().escalate(d, maintenanceArchiveSource, fmt.Sprintf(
			"scheduled_maintenance: history archive: cannot read the watermarks: %v\n"+
				"No database's dolt_log or events was archived this run. A flatten discards history it has not written out.",
			err))
		return maintenanceGCResult{outcome: gcOutcomeCompleted}, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.logger.Printf("scheduled_maintenance: archive: cannot create %s: %v — no history archived this run", dir, err)
		d.maintenance().escalate(d, maintenanceArchiveSource, fmt.Sprintf(
			"scheduled_maintenance: history archive: cannot create %s: %v\n"+
				"No database's dolt_log or events was archived this run. A flatten discards history it has not written out.",
			dir, err))
		return maintenanceGCResult{outcome: gcOutcomeCompleted}, nil
	}

	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, maintenanceArchiveTimeout)
	defer cancel()

	var failed []string
	unarchived := map[string]bool{}
	for _, db := range databases {
		if err := d.maintenanceArchiveCommits(ctx, dir, db, st.ArchiveWatermarks[db]); err != nil {
			failed = append(failed, db+" dolt_log: "+err.Error())
			unarchived[db] = true
			d.logger.Printf("scheduled_maintenance: archive %s dolt_log: FAILED: %v — watermark kept", db, err)
		}
		if err := d.maintenanceArchiveEvents(ctx, dir, db, st.ArchiveEventsWatermarks[db]); err != nil {
			failed = append(failed, db+" events: "+err.Error())
			unarchived[db] = true
			d.logger.Printf("scheduled_maintenance: archive %s events: FAILED: %v — watermark kept", db, err)
		}
	}
	if len(failed) > 0 {
		// One attention item per run, however many exports failed.
		d.maintenance().escalate(d, maintenanceArchiveSource, fmt.Sprintf(
			"scheduled_maintenance: history archive failed for %d export(s):\n%s\n"+
				"Each keeps its previous watermark, so the next run exports those rows again. "+
				"The gc of those databases is skipped until their archive completes. Archiving to %s.",
			len(failed), strings.Join(failed, "\n"), dir))
	}
	return maintenanceGCResult{outcome: gcOutcomeCompleted}, unarchived
}

// gcDatabasesAfterArchive is the databases a cycle may gc: every database whose
// archive completed. A database whose export failed skips its gc, because
// history is not collected where its archive step did not finish. Each skip is
// logged by name.
func (d *Daemon) gcDatabasesAfterArchive(databases []string, unarchived map[string]bool) []string {
	if len(unarchived) == 0 {
		return databases
	}
	out := make([]string, 0, len(databases))
	for _, db := range databases {
		if unarchived[db] {
			d.logger.Printf("scheduled_maintenance: gc %s: skipped — its history archive failed this run", db)
			continue
		}
		out = append(out, db)
	}
	return out
}

// maintenanceArchiveDir returns the configured archive directory, empty when
// archiving is off.
func (d *Daemon) maintenanceArchiveDir() (string, error) {
	if d.patrolConfig == nil || d.patrolConfig.Patrols == nil {
		return "", nil
	}
	return d.patrolConfig.Patrols.ScheduledMaintenance.ParseArchiveDir()
}

// maintenanceArchiveCommits exports one database's new commits and then
// advances the commit watermark. The file and the watermark move as one unit:
// a failure after the file is written rolls the appended members back, so the
// next run re-exports them instead of appending them twice.
func (d *Daemon) maintenanceArchiveCommits(ctx context.Context, dir, db string, wm ArchiveWatermark) error {
	if err := validMaintenanceDBName(db); err != nil {
		return err
	}
	if err := wm.Validate(); err != nil {
		return err
	}
	s := d.maintenance()
	commits, err := s.archiveRead(ctx, d, db, wm)
	if err != nil {
		return fmt.Errorf("reading dolt_log: %w", err)
	}
	if len(commits) == 0 {
		return nil // nothing new: no file, and the watermark stays put
	}
	next, err := archiveWatermarkAfter(wm, commits)
	if err != nil {
		return err
	}

	path := filepath.Join(dir, archiveFileName("dolt-log", db, s.now()))
	exp, err := beginArchiveExport(path)
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(commits))
	for _, c := range commits {
		rows = append(rows, []string{c.Hash, c.Committer, c.Date.UTC().Format(time.RFC3339), c.Message})
	}
	if err := exp.append(s, archiveCSVHeader, rows); err != nil {
		return exp.fail(fmt.Errorf("writing %s: %w", path, err))
	}

	if err := recordArchiveWatermark(d.config.TownRoot, db, next); err != nil {
		return exp.fail(fmt.Errorf("the watermark for %s cannot be recorded: %w", path, err))
	}
	d.logger.Printf("scheduled_maintenance: archive %s: %d commit(s) to %s, through %s (%d hash(es) at that date)",
		db, len(commits), path, next.Date.UTC().Format(time.RFC3339), len(next.Hashes))
	return nil
}

// archiveWatermarkAfter is the cursor the commits just written leave behind:
// the newest date written and every hash written at it. The previous
// watermark's hashes join the set when the newest date is still the previous
// one, so the boundary commits are recorded together rather than one per run.
// Hashes are sorted so the state file does not depend on read order.
func archiveWatermarkAfter(prev ArchiveWatermark, commits []archiveCommit) (ArchiveWatermark, error) {
	newest := prev.Date
	for _, c := range commits {
		if c.Hash == "" {
			return ArchiveWatermark{}, fmt.Errorf("dolt_log read returned a commit with no hash")
		}
		if c.Date.IsZero() {
			return ArchiveWatermark{}, fmt.Errorf("dolt_log read returned commit %s with no date", c.Hash)
		}
		if !prev.Date.IsZero() && c.Date.Before(prev.Date) {
			// Writing it would not move the cursor past it, so the next run
			// would append it again. The read is bounded at the watermark's
			// date; a row below it means the read ignored its cursor.
			return ArchiveWatermark{}, fmt.Errorf("dolt_log read returned commit %s dated %s, before the watermark %s",
				c.Hash, c.Date.UTC().Format(time.RFC3339), prev.Date.UTC().Format(time.RFC3339))
		}
		if c.Date.After(newest) {
			newest = c.Date
		}
	}
	set := make(map[string]bool, len(prev.Hashes)+len(commits))
	if !newest.IsZero() && newest.Equal(prev.Date) {
		for _, h := range prev.Hashes {
			set[h] = true
		}
	}
	for _, c := range commits {
		if c.Date.Equal(newest) {
			set[c.Hash] = true
		}
	}
	hashes := make([]string, 0, len(set))
	for h := range set {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	return ArchiveWatermark{Date: newest, Hashes: hashes}, nil
}

// maintenanceArchiveEvents exports one database's events rows in the lookback
// window, one batch at a time, skipping the ids the watermark already holds,
// and advances the watermark once the last batch is on disk. Every batch of the
// export is rolled back together if a later read, write or the watermark record
// fails, so a half-exported table is never left ahead of the watermark for the
// next run to append again.
func (d *Daemon) maintenanceArchiveEvents(parent context.Context, dir, db string, wm EventsArchiveWatermark) error {
	if err := validMaintenanceDBName(db); err != nil {
		return err
	}
	if err := wm.Validate(); err != nil {
		return err
	}
	s := d.maintenance()
	ctx, cancel := context.WithTimeout(parent, maintenanceArchiveExportTimeout)
	defer cancel()

	path := filepath.Join(dir, archiveFileName("events", db, s.now()))
	exp, err := beginArchiveExport(path)
	if err != nil {
		return err
	}
	since, err := wm.Since()
	if err != nil {
		return err
	}
	archived := wm.ArchivedIDs()
	var (
		columns  []string
		after    eventsCursor
		exported int
	)
	for {
		batch, err := s.archiveEventsRead(ctx, d, db, eventsArchiveRead{Since: since, After: after}, maintenanceArchiveEventsBatch)
		if err != nil {
			return exp.fail(fmt.Errorf("reading events: %w", err))
		}
		if len(batch.Rows) == 0 {
			break
		}
		if len(columns) == 0 {
			columns = batch.Columns
			if _, _, err := eventsWatermarkColumns(columns); err != nil {
				return exp.fail(err)
			}
		} else if !slices.Equal(columns, batch.Columns) {
			return exp.fail(fmt.Errorf("events columns changed between batches: %v then %v", columns, batch.Columns))
		}

		// The cursor advances over every row the read returned, including the
		// ones this run must skip: a read that hands back the cursor it was
		// given cannot make progress, and a window made only of archived rows
		// still pages forward instead of stopping or repeating.
		next, err := eventsCursorFrom(columns, batch.Rows[len(batch.Rows)-1])
		if err != nil {
			return exp.fail(err)
		}
		if next == after {
			return exp.fail(fmt.Errorf("events read made no progress past %s/%s", after.CreatedAt, after.ID))
		}
		_, idColumn, err := eventsWatermarkColumns(columns)
		if err != nil {
			return exp.fail(err)
		}
		fresh := make([][]string, 0, len(batch.Rows))
		for _, row := range batch.Rows {
			if archived[row[idColumn]] {
				continue
			}
			fresh = append(fresh, row)
		}
		if len(fresh) > 0 {
			if err := exp.append(s, columns, fresh); err != nil {
				return exp.fail(fmt.Errorf("writing %s: %w", path, err))
			}
			exported += len(fresh)
			nextWM, err := wm.written(columns, fresh)
			if err != nil {
				return exp.fail(err)
			}
			wm = nextWM
		}
		after = next
		if len(batch.Rows) < maintenanceArchiveEventsBatch {
			break
		}
	}
	if !exp.wrote {
		return nil // nothing new: no file, and the watermark stays put
	}

	if err := recordEventsArchiveWatermark(d.config.TownRoot, db, wm); err != nil {
		return exp.fail(fmt.Errorf("the watermark for %s cannot be recorded: %w", path, err))
	}
	d.logger.Printf("scheduled_maintenance: archive %s events: %d row(s) to %s, through %s (%d id(s) in the window)",
		db, exported, path, wm.CreatedAt, len(wm.Archived))
	return nil
}

// archiveExport is one database's export into one archive file. Until the
// caller records the watermark, every member this export appended can be
// undone, so the watermark is the only record of a complete export
// (gt-7nxj4). State it does not cover: a process that dies between a member's
// write and the watermark record leaves that member behind, and the next run
// appends it again (an error, not a crash, is what fail covers).
type archiveExport struct {
	path    string
	existed bool
	start   int64
	wrote   bool
}

// beginArchiveExport records the file's state before the export appends to it.
func beginArchiveExport(path string) (*archiveExport, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &archiveExport{path: path}, nil
	}
	if err != nil {
		return nil, err
	}
	return &archiveExport{path: path, existed: true, start: info.Size()}, nil
}

// append appends one gzip CSV member through the injected append seam,
// returning only after the file is closed, and marks the export as having
// written to it.
func (e *archiveExport) append(s maintenanceSeams, columns []string, rows [][]string) error {
	if err := s.archiveAppend(e.path, columns, rows); err != nil {
		return err
	}
	e.wrote = true
	return nil
}

// fail rolls the export's appended members back and returns err, naming the
// rollback when that fails too. A file the export created is removed so the
// run that retries writes its header again; an existing one is cut back to the
// size it had, which is a whole-member boundary.
func (e *archiveExport) fail(err error) error {
	if !e.wrote {
		return err
	}
	var rollbackErr error
	if !e.existed || e.start == 0 {
		if rollbackErr = os.Remove(e.path); errors.Is(rollbackErr, fs.ErrNotExist) {
			rollbackErr = nil
		}
	} else {
		rollbackErr = os.Truncate(e.path, e.start)
	}
	if rollbackErr != nil {
		return fmt.Errorf("%w (and the partial export could not be rolled back: %v)", err, rollbackErr)
	}
	return err
}

// eventsWatermarkColumns returns the positions of the created_at and id columns
// the events watermark is built from, or an error when the table lacks them.
func eventsWatermarkColumns(columns []string) (createdAt, id int, err error) {
	createdAt, id = -1, -1
	for i, name := range columns {
		switch name {
		case "created_at":
			createdAt = i
		case "id":
			id = i
		}
	}
	if createdAt < 0 || id < 0 {
		return 0, 0, fmt.Errorf("events table has no created_at and id columns: %v", columns)
	}
	return createdAt, id, nil
}

// eventsCursor is one position in the events read: a row's created_at and id,
// the order the read pages in.
type eventsCursor struct {
	CreatedAt string
	ID        string
}

// eventsArchiveRead is where one events read starts: the lookback window's
// lower bound and the row after which it pages. An empty Since starts at the
// table's oldest row; an empty After starts at Since.
type eventsArchiveRead struct {
	Since string
	After eventsCursor
}

// eventsCursorFrom builds the cursor a written row ends at.
func eventsCursorFrom(columns, row []string) (eventsCursor, error) {
	createdAt, id, err := eventsWatermarkColumns(columns)
	if err != nil {
		return eventsCursor{}, err
	}
	if createdAt >= len(row) || id >= len(row) {
		return eventsCursor{}, fmt.Errorf("events row of %d values is missing column %d or %d", len(row), createdAt, id)
	}
	cursor := eventsCursor{CreatedAt: row[createdAt], ID: row[id]}
	if !beadsql.TimestampLiteral(cursor.CreatedAt) {
		return eventsCursor{}, fmt.Errorf("events row created_at %q is not a timestamp", cursor.CreatedAt)
	}
	if cursor.ID == "" {
		return eventsCursor{}, fmt.Errorf("events row with created_at %s has no id", cursor.CreatedAt)
	}
	return cursor, nil
}

// archiveFileName is <kind>-<db>-<UTC date>.csv.gz: one file per database per
// UTC day, appended to when the window runs twice in it.
func archiveFileName(kind, db string, at time.Time) string {
	return fmt.Sprintf("%s-%s-%s.csv.gz", kind, db, at.UTC().Format("20060102"))
}

// appendArchiveTable appends rows, in columns order, to path as one gzip CSV
// member, writing the column names as the header when it creates the file. It
// returns only after the file is closed, so a caller that advances a watermark
// on success never advances it past a row that did not reach the disk.
func appendArchiveTable(path string, columns []string, rows [][]string) error {
	return appendArchiveMember(path, columns, rows, openArchiveForAppend)
}

// archiveAppendTarget is the file appendArchiveMember appends to: *os.File in
// production, a failing writer in the rollback test.
type archiveAppendTarget interface {
	io.Writer
	Truncate(size int64) error
	Sync() error
	Close() error
}

// openArchiveForAppend opens path for a gzip member to be appended.
func openArchiveForAppend(path string) (archiveAppendTarget, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// appendArchiveMember encodes the rows into one complete gzip member and
// appends it in one write. A failed write or sync is rolled back: an existing
// file is cut to the size it had, and one this append created is removed so the
// next run writes its header again. Either way a partial member never leaves
// the day's archive unreadable or a headerless file behind.
func appendArchiveMember(path string, columns []string, rows [][]string, open func(string) (archiveAppendTarget, error)) (err error) {
	info, statErr := os.Stat(path)
	newFile := errors.Is(statErr, fs.ErrNotExist)
	if statErr != nil && !newFile {
		return statErr
	}
	var start int64
	if !newFile {
		start = info.Size()
	}
	// A zero-length file has no header yet, whatever its name says.
	header := newFile || start == 0
	member, err := archiveMember(columns, rows, header)
	if err != nil {
		return err
	}

	f, err := open(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	rollback := func(cause error) error {
		var rerr error
		if newFile || start == 0 {
			rerr = os.Remove(path)
			if errors.Is(rerr, fs.ErrNotExist) {
				rerr = nil
			}
		} else {
			rerr = f.Truncate(start)
		}
		if rerr != nil {
			return fmt.Errorf("%w (and the partial append could not be rolled back: %v)", cause, rerr)
		}
		return cause
	}
	if _, err := f.Write(member); err != nil {
		return rollback(err)
	}
	if err := f.Sync(); err != nil {
		return rollback(err)
	}
	return nil
}

// archiveMember encodes rows as one gzip CSV member, the header included when
// the file is new.
func archiveMember(columns []string, rows [][]string, newFile bool) ([]byte, error) {
	for i, row := range rows {
		if len(row) != len(columns) {
			return nil, fmt.Errorf("row %d has %d values, want the %d columns", i, len(row), len(columns))
		}
	}
	var buf bytes.Buffer
	err := writeArchiveMember(&buf, columns, rows, newFile)
	return buf.Bytes(), err
}

// writeArchiveMember writes one gzip CSV member to dst. The caller has checked
// that every row has the columns' width. Its failures are the CSV and gzip
// writers', which a test drives with a destination that refuses the write.
func writeArchiveMember(dst io.Writer, columns []string, rows [][]string, newFile bool) error {
	gz := gzip.NewWriter(dst)
	w := csv.NewWriter(gz)
	if newFile {
		if err := w.Write(columns); err != nil {
			return err
		}
	}
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return gz.Close()
}

// recordArchiveWatermark advances one database's commit archive watermark.
func recordArchiveWatermark(townRoot, db string, wm ArchiveWatermark) error {
	_, err := updateMaintenanceGCState(townRoot, func(st *maintenanceGCState) {
		st.ArchiveWatermarks[db] = wm
	})
	return err
}

// recordEventsArchiveWatermark advances one database's events archive
// watermark.
func recordEventsArchiveWatermark(townRoot, db string, wm EventsArchiveWatermark) error {
	_, err := updateMaintenanceGCState(townRoot, func(st *maintenanceGCState) {
		st.ArchiveEventsWatermarks[db] = wm
	})
	return err
}

// LoadArchiveWatermarks returns daemon/maintenance_state.json's per-database
// archive watermarks for `gt dolt status`. An unreadable state file is an error
// rather than an empty set, so the status reports it instead of every database
// reading as never archived.
func LoadArchiveWatermarks(townRoot string) (ArchiveWatermarks, error) {
	st, err := loadMaintenanceGCState(townRoot)
	if err != nil {
		return ArchiveWatermarks{}, err
	}
	return ArchiveWatermarks{Commits: st.ArchiveWatermarks, Events: st.ArchiveEventsWatermarks}, nil
}

// archiveDatePart matches the UTC date an archive file is named by.
var archiveDatePart = regexp.MustCompile(`^\d{8}$`)

// LoadArchivedCommitHashes returns every commit hash db's history archive
// holds: the commit_hash column of each dolt-log-<db>-<date>.csv.gz directly
// under dir. It is the pre-flatten check's authority (gt dolt status): a
// commit in dolt_log whose hash is not here is not archived, whatever its
// date.
//
// A file that cannot be read, or a name that is shaped like an archive file
// but carries no date, is an error: a partial set would let the check call an
// unarchived commit safe. A directory with no file for db is an empty set, not
// an error: that database has archived nothing yet.
func LoadArchivedCommitHashes(dir, db string) (map[string]struct{}, error) {
	if err := validMaintenanceDBName(db); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the history archive %s: %w", dir, err)
	}
	prefix := "dolt-log-" + db + "-"
	out := map[string]struct{}{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".csv.gz") {
			continue
		}
		if entry.IsDir() {
			return nil, fmt.Errorf("%s is a directory, not an archive file", filepath.Join(dir, name))
		}
		if day := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".csv.gz"); !archiveDatePart.MatchString(day) {
			return nil, fmt.Errorf("%s is named like an archive file but carries no UTC date", filepath.Join(dir, name))
		}
		hashes, err := archivedCommitHashesFromFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		for _, hash := range hashes {
			out[hash] = struct{}{}
		}
	}
	return out, nil
}

// archivedCommitHashesFromFile reads one commit archive file's commit_hash
// column, every gzip member in it. The reader's close runs even on an error
// path, and its error is reported only when the read itself found nothing
// wrong.
func archivedCommitHashesFromFile(path string) (out []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s as gzip: %w", path, err)
	}
	defer func() {
		if cerr := gz.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("reading %s: %w", path, cerr)
		}
	}()
	r := csv.NewReader(gz)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("reading %s's header: %w", path, err)
	}
	hashColumn := slices.Index(header, "commit_hash")
	if hashColumn < 0 {
		return nil, fmt.Errorf("%s has no commit_hash column: %v", path, header)
	}
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		if hashColumn >= len(row) || row[hashColumn] == "" {
			return nil, fmt.Errorf("%s has a row with no commit_hash", path)
		}
		out = append(out, row[hashColumn])
	}
	return out, nil
}

// doltArchiveLog reads db's dolt_log rows after the watermark, oldest first by
// (date, commit_hash). It is the commit archive's only SQL, a read-only SELECT
// through the running server. The date bound is inclusive so every commit at
// the cursor's date is read; the hashes the watermark already holds are then
// dropped, leaving exactly the commits not yet archived.
func (d *Daemon) doltArchiveLog(ctx context.Context, db string, after ArchiveWatermark) ([]archiveCommit, error) {
	if err := validMaintenanceDBName(db); err != nil {
		return nil, err
	}
	if err := after.Validate(); err != nil {
		return nil, err
	}
	query := "SELECT commit_hash, committer, date, message FROM dolt_log"
	var args []any
	if !after.Date.IsZero() {
		query += " WHERE date >= ?"
		args = append(args, after.Date)
	}
	query += " ORDER BY date ASC, commit_hash ASC"

	conn, err := d.maintenance().archiveOpenDB(d.maintenanceDSN(db, maintenanceArchiveReadTimeout) + "&parseTime=true")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("dolt_log read: timeout: %w", err)
		}
		return nil, fmt.Errorf("dolt_log read: %w", err)
	}
	defer rows.Close()

	var out []archiveCommit
	for rows.Next() {
		var (
			c       archiveCommit
			message sql.NullString
		)
		if err := rows.Scan(&c.Hash, &c.Committer, &c.Date, &message); err != nil {
			return nil, fmt.Errorf("dolt_log read: %w", err)
		}
		c.Message = encodeArchiveCell(message)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dolt_log read: %w", err)
	}
	return newArchiveCommits(out, after), nil
}

// newArchiveCommits drops the commits a watermark already holds at its date.
// The read's date bound is inclusive, so the boundary commits come back with
// the new ones; without this filter every run would append them to the day's
// file again.
func newArchiveCommits(commits []archiveCommit, wm ArchiveWatermark) []archiveCommit {
	if len(wm.Hashes) == 0 {
		return commits
	}
	out := commits[:0]
	for _, c := range commits {
		if wm.covers(c.Hash) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// doltArchiveEvents reads up to limit of db's events rows at or after the
// lookback window's start (and after the paging cursor when one is set), oldest
// first. It is the events archive's only SQL, a read-only SELECT through the
// running server, and every column is read in the table's own order.
func (d *Daemon) doltArchiveEvents(ctx context.Context, db string, read eventsArchiveRead, limit int) (archiveEvents, error) {
	if err := validMaintenanceDBName(db); err != nil {
		return archiveEvents{}, err
	}
	if limit <= 0 {
		return archiveEvents{}, fmt.Errorf("events read limit %d is not positive", limit)
	}
	if read.Since != "" && !beadsql.TimestampLiteral(read.Since) {
		return archiveEvents{}, fmt.Errorf("events read since %q is not a timestamp", read.Since)
	}
	if read.After.CreatedAt != "" || read.After.ID != "" {
		if _, err := eventsCursorFrom([]string{"created_at", "id"}, []string{read.After.CreatedAt, read.After.ID}); err != nil {
			return archiveEvents{}, err
		}
	}
	query, err := beadsql.ArchiveEventsSince(db, read.Since, read.After.CreatedAt, read.After.ID, limit).Inline()
	if err != nil {
		return archiveEvents{}, err
	}
	// No parseTime: the row is archived as the text Dolt renders it, and every
	// column is scanned as text.
	conn, err := d.maintenance().archiveOpenDB(d.maintenanceDSN(db, maintenanceArchiveReadTimeout))
	if err != nil {
		return archiveEvents{}, err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return archiveEvents{}, fmt.Errorf("events read: timeout: %w", err)
		}
		return archiveEvents{}, fmt.Errorf("events read: %w", err)
	}
	defer rows.Close()

	return eventsArchiveRows(rows)
}

// eventsArchiveRows reads every column of an open result set into one events
// archive batch, oldest first.
func eventsArchiveRows(rows *sql.Rows) (archiveEvents, error) {
	columns, err := rows.Columns()
	if err != nil {
		return archiveEvents{}, fmt.Errorf("events read: %w", err)
	}
	out := archiveEvents{Columns: columns}
	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return archiveEvents{}, fmt.Errorf("events read: %w", err)
		}
		row := make([]string, len(columns))
		for i, v := range values {
			row[i] = encodeArchiveCell(v)
		}
		out.Rows = append(out.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return archiveEvents{}, fmt.Errorf("events read: %w", err)
	}
	return out, nil
}

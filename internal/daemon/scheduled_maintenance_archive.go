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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
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

	// maintenanceArchiveExportTimeout bounds one database's whole export, which
	// for a first events export is many batch reads.
	maintenanceArchiveExportTimeout = 15 * time.Minute

	// maintenanceArchiveEventsBatch is how many events rows one read holds.
	// A first export reads the whole table, so it is read in batches the
	// daemon can hold in memory and appended batch by batch.
	maintenanceArchiveEventsBatch = 5000

	// maintenanceArchiveDirKey names the setting in log and error text as the
	// daemon config file spells it.
	maintenanceArchiveDirKey = "patrols.scheduled_maintenance.archive_dir"

	maintenanceArchiveSource = "scheduled_maintenance"
)

// archiveCSVHeader is the commit file's column order (gt-7nxj4).
var archiveCSVHeader = []string{"commit_hash", "committer", "date", "message"}

// ArchiveWatermark is the commit a database's history archive last wrote out:
// its hash, and its dolt_log date so the lower bound survives a flatten that
// drops the commit itself.
type ArchiveWatermark struct {
	Hash string    `json:"hash"`
	Date time.Time `json:"date"`
}

// EventsArchiveWatermark is the newest events row a database's history archive
// last wrote out, in the order the archive writes rows. The zero value means
// nothing has been archived yet.
type EventsArchiveWatermark struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

// Validate reports whether the watermark can bound a query: a zero watermark
// (nothing archived yet) is valid, and so is a created_at Dolt can compare and
// a numeric id. Text it cannot compare is refused rather than matched against
// no row, which would read as "nothing new" (gt-7nxj4).
func (w EventsArchiveWatermark) Validate() error {
	if w.CreatedAt == "" && w.ID == "" {
		return nil
	}
	if !beadsql.TimestampLiteral(w.CreatedAt) {
		return fmt.Errorf("events archive watermark created_at %q is not a timestamp", w.CreatedAt)
	}
	if _, err := strconv.ParseInt(w.ID, 10, 64); err != nil {
		return fmt.Errorf("events archive watermark id %q is not an integer", w.ID)
	}
	return nil
}

// ArchiveWatermarks is one town's history archive cursors, per database.
type ArchiveWatermarks struct {
	Commits map[string]ArchiveWatermark
	Events  map[string]EventsArchiveWatermark
}

// archiveCommit is one dolt_log row the archive writes.
type archiveCommit struct {
	Hash      string
	Committer string
	Date      time.Time
	Message   string
}

// archiveEvents is one batch of events rows: the table's columns in query
// order, and each row's values in that order. A NULL is an empty value.
type archiveEvents struct {
	Columns []string
	Rows    [][]string
}

// maintenanceArchive appends every database's new commits and new events rows
// to the archive. One export failing keeps its watermark and is named in the
// log; the database's other export, the other databases, and the gc still run.
// A failed export does not skip that database's gc either: CALL
// dolt_gc('--full') never rewrites history, and an archive outage must not
// stall the weekly gc.
func (d *Daemon) maintenanceArchive(databases []string) {
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
		return
	}
	if dir == "" {
		// Once per run, not once per database: the step is off, not broken.
		d.logger.Printf("scheduled_maintenance: archive: %s unset — history archiving is off", maintenanceArchiveDirKey)
		return
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
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.logger.Printf("scheduled_maintenance: archive: cannot create %s: %v — no history archived this run", dir, err)
		d.maintenance().escalate(d, maintenanceArchiveSource, fmt.Sprintf(
			"scheduled_maintenance: history archive: cannot create %s: %v\n"+
				"No database's dolt_log or events was archived this run. A flatten discards history it has not written out.",
			dir, err))
		return
	}

	var failed []string
	for _, db := range databases {
		if err := d.maintenanceArchiveCommits(dir, db, st.ArchiveWatermarks[db]); err != nil {
			failed = append(failed, db+" dolt_log: "+err.Error())
			d.logger.Printf("scheduled_maintenance: archive %s dolt_log: FAILED: %v — watermark kept", db, err)
		}
		if err := d.maintenanceArchiveEvents(dir, db, st.ArchiveEventsWatermarks[db]); err != nil {
			failed = append(failed, db+" events: "+err.Error())
			d.logger.Printf("scheduled_maintenance: archive %s events: FAILED: %v — watermark kept", db, err)
		}
	}
	if len(failed) > 0 {
		// One attention item per run, however many exports failed.
		d.maintenance().escalate(d, maintenanceArchiveSource, fmt.Sprintf(
			"scheduled_maintenance: history archive failed for %d export(s):\n%s\n"+
				"Each keeps its previous watermark, so the next run exports those rows again. "+
				"Archiving to %s.",
			len(failed), strings.Join(failed, "\n"), dir))
	}
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
// advances the commit watermark.
func (d *Daemon) maintenanceArchiveCommits(dir, db string, wm ArchiveWatermark) error {
	if err := validMaintenanceDBName(db); err != nil {
		return err
	}
	s := d.maintenance()
	ctx, cancel := d.maintenanceArchiveContext()
	defer cancel()

	commits, err := s.archiveRead(ctx, d, db, wm)
	if err != nil {
		return fmt.Errorf("reading dolt_log: %w", err)
	}
	if len(commits) == 0 {
		return nil // nothing new: no file, and the watermark stays put
	}

	path := filepath.Join(dir, archiveFileName("dolt-log", db, s.now()))
	if err := appendArchiveCSV(path, commits); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	last := commits[len(commits)-1]
	if err := recordArchiveWatermark(d.config.TownRoot, db, ArchiveWatermark{Hash: last.Hash, Date: last.Date}); err != nil {
		return fmt.Errorf("%s is written but the watermark cannot be recorded (the next run exports these commits again): %w", path, err)
	}
	d.logger.Printf("scheduled_maintenance: archive %s: %d commit(s) to %s, through %s",
		db, len(commits), path, last.Date.UTC().Format(time.RFC3339))
	return nil
}

// maintenanceArchiveEvents exports one database's new events rows, one batch
// at a time, and advances the events watermark once the last batch is on disk.
func (d *Daemon) maintenanceArchiveEvents(dir, db string, wm EventsArchiveWatermark) error {
	if err := validMaintenanceDBName(db); err != nil {
		return err
	}
	if err := wm.Validate(); err != nil {
		return err
	}
	s := d.maintenance()
	ctx, cancel := d.maintenanceArchiveContext()
	defer cancel()

	path := filepath.Join(dir, archiveFileName("events", db, s.now()))
	var (
		columns  []string
		last     []string
		exported int
		wrote    bool
	)
	for {
		batch, err := s.archiveEventsRead(ctx, d, db, wm, maintenanceArchiveEventsBatch)
		if err != nil {
			return fmt.Errorf("reading events: %w", err)
		}
		if len(batch.Rows) == 0 {
			break
		}
		if len(columns) == 0 {
			columns = batch.Columns
			if _, _, err := eventsWatermarkColumns(columns); err != nil {
				return err
			}
		} else if !slices.Equal(columns, batch.Columns) {
			return fmt.Errorf("events columns changed between batches: %v then %v", columns, batch.Columns)
		}
		if err := appendArchiveTable(path, columns, batch.Rows); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		wrote = true
		exported += len(batch.Rows)
		last = batch.Rows[len(batch.Rows)-1]

		next, err := eventsWatermarkFrom(columns, last)
		if err != nil {
			return err
		}
		if next == wm {
			// A read that hands back the cursor it was given cannot advance the
			// export; stopping here keeps the loop bounded.
			return fmt.Errorf("events read made no progress past %s/%s", wm.CreatedAt, wm.ID)
		}
		wm = next
		if len(batch.Rows) < maintenanceArchiveEventsBatch {
			break
		}
	}
	if !wrote {
		return nil // nothing new: no file, and the watermark stays put
	}

	if err := recordEventsArchiveWatermark(d.config.TownRoot, db, wm); err != nil {
		return fmt.Errorf("%s is written but the watermark cannot be recorded (the next run exports these rows again): %w", path, err)
	}
	d.logger.Printf("scheduled_maintenance: archive %s events: %d row(s) to %s, through %s id %s",
		db, exported, path, wm.CreatedAt, wm.ID)
	return nil
}

// maintenanceArchiveContext is the context one database's archive reads run
// under: the daemon's, so a shutdown cancels them, bounded by
// maintenanceArchiveExportTimeout.
func (d *Daemon) maintenanceArchiveContext() (context.Context, context.CancelFunc) {
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, maintenanceArchiveExportTimeout)
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

// eventsWatermarkFrom builds the watermark a written row ends at.
func eventsWatermarkFrom(columns, row []string) (EventsArchiveWatermark, error) {
	createdAt, id, err := eventsWatermarkColumns(columns)
	if err != nil {
		return EventsArchiveWatermark{}, err
	}
	if createdAt >= len(row) || id >= len(row) {
		return EventsArchiveWatermark{}, fmt.Errorf("events row of %d values is missing column %d or %d", len(row), createdAt, id)
	}
	wm := EventsArchiveWatermark{CreatedAt: row[createdAt], ID: row[id]}
	if err := wm.Validate(); err != nil {
		return EventsArchiveWatermark{}, err
	}
	return wm, nil
}

// archiveFileName is <kind>-<db>-<UTC date>.csv.gz: one file per database per
// UTC day, appended to when the window runs twice in it.
func archiveFileName(kind, db string, at time.Time) string {
	return fmt.Sprintf("%s-%s-%s.csv.gz", kind, db, at.UTC().Format("20060102"))
}

// appendArchiveCSV appends commits to path as one gzip CSV member.
func appendArchiveCSV(path string, commits []archiveCommit) error {
	rows := make([][]string, 0, len(commits))
	for _, c := range commits {
		rows = append(rows, []string{c.Hash, c.Committer, c.Date.UTC().Format(time.RFC3339), c.Message})
	}
	return appendArchiveTable(path, archiveCSVHeader, rows)
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
	Stat() (os.FileInfo, error)
	Truncate(size int64) error
	Sync() error
	Close() error
}

// openArchiveForAppend opens path for a gzip member to be appended.
func openArchiveForAppend(path string) (archiveAppendTarget, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// appendArchiveMember encodes the rows into one complete gzip member and
// appends it in one write. A failed write is rolled back to the size the file
// had before it, so a partial member never leaves the day's archive unreadable:
// the next run re-exports the rows and appends a whole member.
func appendArchiveMember(path string, columns []string, rows [][]string, open func(string) (archiveAppendTarget, error)) (err error) {
	_, statErr := os.Stat(path)
	newFile := errors.Is(statErr, fs.ErrNotExist)
	if statErr != nil && !newFile {
		return statErr
	}
	member, err := archiveMember(columns, rows, newFile)
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
	info, err := f.Stat()
	if err != nil {
		return err
	}
	start := info.Size()
	if _, err := f.Write(member); err != nil {
		if terr := f.Truncate(start); terr != nil {
			return fmt.Errorf("%w (and the partial append could not be rolled back: %v)", err, terr)
		}
		return err
	}
	return f.Sync()
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

// doltArchiveLog reads db's dolt_log rows after the watermark, oldest first.
// It is the commit archive's only SQL, a read-only SELECT through the running
// server.
func (d *Daemon) doltArchiveLog(ctx context.Context, db string, after ArchiveWatermark) ([]archiveCommit, error) {
	if err := validMaintenanceDBName(db); err != nil {
		return nil, err
	}
	conn, err := d.maintenance().archiveOpenDB(d.maintenanceDSN(db, maintenanceArchiveReadTimeout) + "&parseTime=true")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	query := "SELECT commit_hash, committer, date, message FROM dolt_log"
	var args []any
	if after.Hash != "" {
		// Newer is later by (date, commit_hash), the order the archive writes
		// in. The date is kept beside the hash so the bound survives a flatten
		// that drops the commit.
		query += " WHERE date > ? OR (date = ? AND commit_hash > ?)"
		args = append(args, after.Date, after.Date, after.Hash)
	}
	query += " ORDER BY date ASC, commit_hash ASC"

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
		c.Message = message.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dolt_log read: %w", err)
	}
	return out, nil
}

// doltArchiveEvents reads up to limit of db's events rows after the watermark,
// oldest first. It is the events archive's only SQL, a read-only SELECT through
// the running server, and every column is read in the table's own order.
func (d *Daemon) doltArchiveEvents(ctx context.Context, db string, after EventsArchiveWatermark, limit int) (archiveEvents, error) {
	if err := validMaintenanceDBName(db); err != nil {
		return archiveEvents{}, err
	}
	if limit <= 0 {
		return archiveEvents{}, fmt.Errorf("events read limit %d is not positive", limit)
	}
	if err := after.Validate(); err != nil {
		return archiveEvents{}, err
	}
	query, err := beadsql.ArchiveEvents(db, after.CreatedAt, after.ID, limit).Inline()
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
			row[i] = v.String
		}
		out.Rows = append(out.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return archiveEvents{}, fmt.Errorf("events read: %w", err)
	}
	return out, nil
}

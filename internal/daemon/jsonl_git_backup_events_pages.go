package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
)

// eventsFullPageRows is the rows one page of a paged full export of the events
// table asks for.
const eventsFullPageRows = 20000

// eventsPageReader is the readPage seam of exportEventsPagesWith for database
// db: each call exports one page of the events table, queried after the bound
// it is given, into a fresh staging directory under the events export's own
// eventsIncomingDir, reads the rows back in file order and removes the
// directory. An export that reports no rows returns an empty page.
func (d *Daemon) eventsPageReader(db, dataDir string, budget time.Duration, exportTable jsonlTableExporter) func(afterCreatedAt, afterID string, limit int) ([]json.RawMessage, error) {
	return func(afterCreatedAt, afterID string, limit int) ([]json.RawMessage, error) {
		if !validDBName.MatchString(db) {
			return nil, fmt.Errorf("events page reader: invalid database name %q", db)
		}
		// The stage is a fresh directory under the events export's own
		// eventsIncomingDir, which the next cycle's clearEventsScratchWith
		// removes whole. The paged read calls this serially and the exporter
		// replaces the shards it is given, so one name serves every page.
		repo, _ := JsonlBackupRepo(d.config.TownRoot, d.patrol())
		stage := filepath.Join(repo, db, eventsIncomingDir, "page")
		if err := os.MkdirAll(stage, 0755); err != nil {
			return nil, fmt.Errorf("%s: creating the events page stage: %w", db, err)
		}
		// The exporter replaces the stage's shards, so the next call reads only
		// its own page; a stage a removal cannot delete is cleared with the
		// export at the next cycle.
		defer func() { _ = os.RemoveAll(stage) }()

		n, err := exportTable("events", beadsql.BackupEventsPage(db, afterCreatedAt, afterID, limit), stage, dataDir, budget)
		if err != nil {
			return nil, fmt.Errorf("%s: exporting the events page after %q: %w", db, afterID, err)
		}
		if n == 0 {
			return nil, nil
		}
		rows, err := readStagedEvents(stage)
		if err != nil {
			return nil, fmt.Errorf("%s: reading the exported events page after %q: %w", db, afterID, err)
		}
		return rows, nil
	}
}

// exportEventsPagesWith writes dir's events export from a paged read: each page
// readPage returns is appended as it arrives, the keyset moves to the page's
// last row, and the read stops after a page shorter than pageRows. It returns
// the rows written, and on an error the rows written before it.
//
// A ctx that is done before a page stops the read there, returning ctx.Err()
// with the rows written so far: no boundary is recorded for the partial file,
// so the next cycle reads the table again rather than append from it.
//
// The paged read replaces the export dir holds: a shard an older export left
// would otherwise be appended to, and the file would hold the rows of both.
func (d *Daemon) exportEventsPagesWith(ctx context.Context, ops eventsExportOps, db, dir string, pageRows int, readPage func(afterCreatedAt, afterID string, limit int) ([]json.RawMessage, error)) (int, error) {
	if pageRows < 1 {
		return 0, fmt.Errorf("%s: an events page of %d rows cannot be read", db, pageRows)
	}
	paths, err := jsonlShardPaths(dir, "events")
	if err != nil {
		return 0, fmt.Errorf("%s: listing the events export: %w", db, err)
	}
	for _, path := range paths {
		if err := ops.remove(path); err != nil {
			return 0, fmt.Errorf("%s: clearing %s: %w", db, filepath.Base(path), err)
		}
	}
	// The append needs a shard to append to, and a full read starts from an
	// empty file rather than an absent one.
	first := filepath.Join(dir, jsonlShardName("events", 1))
	if err := ops.writeFile(first, nil, 0644); err != nil {
		return 0, fmt.Errorf("%s: creating %s: %w", db, filepath.Base(first), err)
	}

	state := eventsExportState{Version: eventsExportStateVersion}
	written := 0
	afterCreatedAt, afterID := "", ""
	for {
		if err := ctx.Err(); err != nil {
			return written, fmt.Errorf("%s: events: the paged read stopped after %d rows: %w", db, written, err)
		}
		rows, err := readPage(afterCreatedAt, afterID, pageRows)
		if err != nil {
			return written, fmt.Errorf("%s: reading the events page after %q: %w", db, afterID, err)
		}
		if len(rows) > 0 {
			createdAt, id, err := eventsPageKeyset(rows[len(rows)-1])
			if err != nil {
				return written, fmt.Errorf("%s: %w", db, err)
			}
			// A full page that ends where it was read from is never followed
			// by a shorter one, so the loop would not end.
			if len(rows) == pageRows && afterID != "" && !eventsRowLess(eventsRow{createdAt: afterCreatedAt, id: afterID}, eventsRow{createdAt: createdAt, id: id}) {
				return written, fmt.Errorf("%s: the page ending at (%s, %s) does not move the keyset (%s, %s) on", db, createdAt, id, afterCreatedAt, afterID)
			}
			n, err := appendEventRowsWith(ops, dir, rows, &state, jsonlShardMaxBytes, jsonlShardHardLimit)
			if err != nil {
				return written, fmt.Errorf("%s: appending the events page ending at (%s, %s): %w", db, createdAt, id, err)
			}
			written += n
			afterCreatedAt, afterID = createdAt, id
		}
		if len(rows) < pageRows {
			return written, nil
		}
	}
}

// exportEventsFullPaged replaces dir's events export with a read of the whole
// events table in pages of pageRows. budget is the whole time that read may
// run, as it is for a single read: each page is given what is left of it, and a
// page with none left is refused. The read also stops before a page when the
// daemon's context is done; a Daemon built without one (tests) has only its
// budget to end it.
//
// A read that stops or fails returns the error and leaves the export partial,
// as exportEventsPagesWith does. The backup cycle unstages a database whose
// export failed, so its committed copy stays.
func (d *Daemon) exportEventsFullPaged(ops eventsExportOps, db, dir, dataDir string, budget time.Duration, pageRows int, exportTable jsonlTableExporter) (int, error) {
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	start := d.clk().Now()
	return d.exportEventsPagesWith(ctx, ops, db, dir, pageRows, func(afterCreatedAt, afterID string, limit int) ([]json.RawMessage, error) {
		remaining := budget - d.clk().Now().Sub(start)
		if remaining <= 0 {
			return nil, fmt.Errorf("the events full export spent its %s budget", budget)
		}
		return d.eventsPageReader(db, dataDir, remaining, exportTable)(afterCreatedAt, afterID, limit)
	})
}

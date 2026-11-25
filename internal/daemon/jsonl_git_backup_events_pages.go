package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
)

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

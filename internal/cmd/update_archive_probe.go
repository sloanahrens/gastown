package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// archiveGap sums, over databases, how far the Dolt history archive is behind
// in commits and in events. It is the number pair the update plan decides its
// archive work on, so every database is counted and none is skipped: a
// database with no watermark still gets both counts (the reader answers it the
// way `gt dolt status` does). A reader error returns zeros with the
// database and the kind named — a partial sum, or a zero, would let the plan
// read an unreadable archive as caught up.
func archiveGap(ctx context.Context, reader archiveBehindReader, wms daemon.ArchiveWatermarks, databases []string) (commits, events int64, err error) {
	for _, db := range databases {
		behind, err := reader.commitsBehind(ctx, db, wms.Commits[db])
		if err != nil {
			return 0, 0, fmt.Errorf("counting commits behind in database %q: %w", db, err)
		}
		commits += behind
		behind, err = reader.eventsBehind(ctx, db, wms.Events[db])
		if err != nil {
			return 0, 0, fmt.Errorf("counting events behind in database %q: %w", db, err)
		}
		if behind == eventsBehindUncounted {
			// The table holds rows the reader did not count: at least one
			// event is behind, never the sentinel's -1.
			behind = 1
		}
		events += behind
	}
	return commits, events, nil
}

// newArchiveGapFunc returns the plan's archive probe: how many commits and
// events the history archive is behind, summed over the town's databases. It
// takes its inputs from the same calls `gt dolt status` makes — the archive
// directory from the daemon config, the watermarks from the daemon state, and
// the database list and Dolt config from the status command's own calls — and
// counts through the same reader the status notes use.
//
// An archive that is switched off (no maintenance.archive_dir) is reported as
// (0, 0, nil): there is no archive for it to be behind. Every other failure is
// an error, so the plan never reads an archive it could not inspect as caught
// up.
func newArchiveGapFunc(townRoot string) func(ctx context.Context) (commits, events int64, err error) {
	return func(ctx context.Context) (int64, int64, error) {
		dir, err := archiveDirForStatus(townRoot)
		if err != nil {
			return 0, 0, err
		}
		if dir == "" {
			return 0, 0, nil
		}
		wms, err := daemon.LoadArchiveWatermarks(townRoot)
		if err != nil {
			return 0, 0, fmt.Errorf("reading the archive watermarks: %w", err)
		}
		config := doltserver.DefaultConfig(townRoot)
		databases, err := doltserver.ListDatabases(townRoot)
		if err != nil {
			return 0, 0, fmt.Errorf("listing the Dolt databases: %w", err)
		}
		reader := doltArchiveReader{config: config, archivedHashes: archivedHashesLoader(townRoot, databases)}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return archiveGap(ctx, reader, wms, databases)
	}
}

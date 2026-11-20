package jobs

// The disk maintenance rows: one per target in the ledger the disk maintenance
// patrol writes (internal/daemon/disk_maintenance.go, bead gt-zxwd3h). The
// ledger is another writer's append-only JSON-lines file, and this reads its
// bytes itself: Ledger.Tail takes a lock file beside it, which the dashboard's
// read-only mount of the town cannot create.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/diskmaint"
)

const (
	// diskmaintJobPrefix names a target's row.
	diskmaintJobPrefix = "diskmaint:"
	// diskmaintLedgerJobName is the row that stands for the ledger itself when
	// it cannot be read or holds no line this list can attribute to a target,
	// so a damaged ledger is an error row and never an empty table.
	diskmaintLedgerJobName = "diskmaint:ledger"
	// diskmaintModeApply is the mode of a run that changed the disk; only an
	// applied run becomes a target's row, since a dry run changed nothing.
	diskmaintModeApply = "apply"
	// diskmaintSkippedStat names the count of lines the read could not
	// attribute to a target, so the damage is visible on the row that reports
	// it rather than a shorter history passing as the whole one.
	diskmaintSkippedStat = "skipped_lines"
)

// diskmaintRead is one read of the ledger: the newest applied entry per target,
// and how many non-blank lines decoded to no target.
type diskmaintRead struct {
	newest  map[string]diskmaint.LedgerEntry
	skipped int
}

// diskmaintRows returns one row per target in the town's disk maintenance
// ledger, each the target's newest applied run with its outcome and figures.
// An unreadable ledger, and a ledger whose every line decodes to no target,
// give one failed unknown row named diskmaint:ledger instead of nothing; a
// missing ledger, and one whose every entry is a dry run, give no row. A line
// that does not parse, or an entry naming no target, is skipped and counted: a
// target row beside it still stands, and the failed unknown ledger row reports
// the count, so damage never passes as a shorter complete history.
func diskmaintRows(fsys fs.FS, now time.Time) []Job {
	read, err := readDiskmaintLedger(fsys, daemon.DiskMaintLedgerRelPath)
	if err != nil {
		return []Job{diskmaintLedgerRow(0)}
	}
	if len(read.newest) == 0 {
		if read.skipped == 0 {
			return nil
		}
		return []Job{diskmaintLedgerRow(read.skipped)}
	}
	names := slices.Sorted(maps.Keys(read.newest))
	rows := make([]Job, 0, len(names)+1)
	for _, name := range names {
		rows = append(rows, diskmaintRow(name, read.newest[name], now))
	}
	if read.skipped > 0 {
		rows = append(rows, diskmaintLedgerRow(read.skipped))
	}
	return rows
}

// diskmaintRow is one target's row: the outcome its newest applied run ended
// with, and that run's completion and figures.
func diskmaintRow(target string, e diskmaint.LedgerEntry, now time.Time) Job {
	j := Job{
		Name:    diskmaintJobPrefix + target,
		Kind:    config.DaemonStepPatrol,
		Outcome: diskmaintOutcome(e),
		Stats:   map[string]int64{"done": int64(e.Done), "bytes_freed": e.BytesFreed},
	}
	if !e.TS.IsZero() {
		at := e.TS
		j.LastRun = &at
	}
	j.State = stateOf(j, false, false, now)
	return j
}

// diskmaintOutcome is how an applied run ended. A run that recorded an error
// failed even if it also refused or deferred part of its work.
func diskmaintOutcome(e diskmaint.LedgerEntry) Outcome {
	switch {
	case len(e.Errors) > 0:
		return OutcomeFailed
	case e.Refused != "":
		return OutcomeRefused
	case e.Deferred != "":
		return OutcomeDeferred
	}
	return OutcomeOK
}

// diskmaintLedgerRow is the ledger's own row: unknown and failed, because
// neither an unreadable ledger nor one that decodes to nothing says what the
// targets did. skipped is the count of lines it could not attribute, 0 when it
// could not be read at all.
func diskmaintLedgerRow(skipped int) Job {
	j := Job{
		Name:    diskmaintLedgerJobName,
		Kind:    config.DaemonStepPatrol,
		State:   StateUnknown,
		Outcome: OutcomeFailed,
	}
	if skipped > 0 {
		j.Stats = map[string]int64{diskmaintSkippedStat: int64(skipped)}
	}
	return j
}

// readDiskmaintLedger reads the ledger's rotation and its live file through
// fsys. A file that does not exist holds nothing; a file that cannot be read is
// an error. The rotation is read first, so the last applied entry for a target
// in file order is its newest, as an append-only ledger writes them.
func readDiskmaintLedger(fsys fs.FS, rel string) (diskmaintRead, error) {
	var read diskmaintRead
	for _, name := range []string{rel + ".1", rel} {
		data, err := fs.ReadFile(fsys, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return diskmaintRead{}, fmt.Errorf("read the disk maintenance ledger %s: %w", name, err)
		}
		readDiskmaintLines(&read, data)
	}
	return read, nil
}

// readDiskmaintLines decodes one file into the read. A line that does not parse
// (a write torn by a crash) and an entry that names no target are counted as
// skipped: neither says anything about a target, and passing either over in
// silence would report a shorter history as the whole one.
func readDiskmaintLines(read *diskmaintRead, data []byte) {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e diskmaint.LedgerEntry
		if err := json.Unmarshal(line, &e); err != nil || e.Target == "" {
			read.skipped++
			continue
		}
		if e.Mode != diskmaintModeApply {
			continue
		}
		if read.newest == nil {
			read.newest = make(map[string]diskmaint.LedgerEntry)
		}
		read.newest[e.Target] = e
	}
}

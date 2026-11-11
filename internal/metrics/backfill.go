package metrics

import (
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
)

// The backfill: the regime changes the town made while the runtime record did
// not exist, written to regime-events-backfill-<day>.jsonl for the reader to
// merge beside it. The install events are not here: RecordInstallReceipts
// records them from the receipts into the record itself, and the file holds
// what no receipt carries — the moves of the seats and the daemon, and the seat
// models the settings put in force at the move.
//
// The file is named for the UTC day it is written. Running the backfill again
// writes the same day's file again with the same events; running it on a later
// day writes a new file that restates them, and the reader counts a record
// repeated across the runtime files once (same kind, ts and after_ref).

// regimeBackfillDayLayout is the day form the backfill file's name carries.
const regimeBackfillDayLayout = "20060102"

// regimeSandboxMoveAt is the day the seats moved from the host into sandboxes.
// It is when the sbx seats in settings/config.json came into force, so it is
// the instant their seat_model events are placed at: the settings hold the
// seats in force, not the day each was configured.
var regimeSandboxMoveAt = time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

// regimeBackfillMoves are the moves the town made before the runtime record
// existed, so the analysis can cut a series at them. The record holds every
// change `gt regime record` wrote since; a later move is recorded there live
// and is not added to this table. A move is known by its day, so its instant is
// that day's UTC midnight, the instant a settings event is placed at.
var regimeBackfillMoves = []RegimeEventRecord{
	{
		TS:     regimeSandboxMoveAt,
		Kind:   "platform",
		Reason: "seats moved from the host into sandboxes",
		Source: "mango-evaluation review",
	},
	{
		TS:     time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
		Kind:   "platform",
		Reason: "daemon moved from the host into the control cluster",
		Source: "mango-evaluation review",
	},
}

// backfillPath is the backfill file for the UTC day of day under town: the
// name the reader merges with the runtime record.
func backfillPath(town string, day time.Time) string {
	return filepath.Join(town, regimeEventsRuntimeDir,
		regimeEventsBackfillPrefix+day.UTC().Format(regimeBackfillDayLayout)+regimeEventsBackfillSuffix)
}

// RegimeBackfill writes town's backfill file for the day of now and returns its
// path and how many events it holds: the moves in regimeBackfillMoves and one
// seat_model event per seat the town's settings put in force at the sandbox
// move, each naming the model its agent config names. A town with no settings
// file has no seats to name and still gets the moves; a settings file that is
// there and cannot be read or parsed fails the call, so a seat list the town
// cannot read is never written as a town with no seats.
func RegimeBackfill(env Env, now time.Time, actor string) (string, int, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", 0, errors.New("regime backfill: no actor to record the events under")
	}
	if env.ReadFile == nil {
		return "", 0, errors.New("regime backfill: Env.ReadFile is unset")
	}
	seats, err := seatModelRecords(env, actor)
	if err != nil {
		return "", 0, fmt.Errorf("regime backfill: %w", err)
	}
	records := make([]RegimeEventRecord, 0, len(regimeBackfillMoves)+len(seats))
	for _, move := range regimeBackfillMoves {
		move.Actor = actor
		records = append(records, move)
	}
	records = append(records, seats...)
	path := backfillPath(env.Town, now)
	n, err := writeRegimeBackfill(path, records)
	if err != nil {
		return path, 0, err
	}
	return path, n, nil
}

// seatModelRecords is one seat_model event per seat in settings/config.json
// whose town agent config names a model, at the sandbox move. A seat whose
// agent names no model is left out, as the snapshot's seat_models column leaves
// it out: its default is the agent's, not a value the settings state.
func seatModelRecords(env Env, actor string) ([]RegimeEventRecord, error) {
	settings, err := configReadSettings(env)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if settings.PolecatPool == nil || len(settings.PolecatPool.Seats) == 0 {
		return nil, nil
	}
	agents := make([]string, 0, len(settings.PolecatPool.Seats))
	for agent := range settings.PolecatPool.Seats {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	records := make([]RegimeEventRecord, 0, len(agents))
	for _, agent := range agents {
		model := configSeatModel(settings, agent)
		if model == "" {
			continue
		}
		records = append(records, RegimeEventRecord{
			TS:       regimeSandboxMoveAt,
			Kind:     "seat_model",
			AfterRef: model,
			Reason:   agent + " seat runs " + model,
			Actor:    actor,
			Source:   filepath.ToSlash(configSettingsFile),
		})
	}
	return records, nil
}

// writeRegimeBackfill writes records to path as the backfill file: the events
// trimmed and validated like the writer's, oldest first, a record repeated in
// records counted once, and one JSON line each. The file is created 0600 and its
// directory 0700, and an existing file is replaced, so the same day's file holds
// the same events however often the backfill runs. It returns the number of
// lines written.
func writeRegimeBackfill(path string, records []RegimeEventRecord) (int, error) {
	lines, n, err := backfillLines(records)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, fmt.Errorf("regime backfill: creating %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("regime backfill: opening %s: %w", path, err)
	}
	written, werr := f.Write(lines)
	cerr := f.Close()
	if werr != nil {
		return 0, fmt.Errorf("regime backfill: writing %s: %w", path, werr)
	}
	if written != len(lines) {
		return 0, fmt.Errorf("regime backfill: writing %s: %w", path, io.ErrShortWrite)
	}
	if cerr != nil {
		return 0, fmt.Errorf("regime backfill: closing %s: %w", path, cerr)
	}
	return n, nil
}

// backfillLines encodes records as the file holds them and returns the bytes and
// the line count.
func backfillLines(records []RegimeEventRecord) ([]byte, int, error) {
	sorted := make([]RegimeEventRecord, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].TS.Before(sorted[j].TS) })
	var buf bytes.Buffer
	seen := make(map[regimeRecordKey]struct{}, len(sorted))
	n := 0
	for _, rec := range sorted {
		rec.Kind = strings.TrimSpace(rec.Kind)
		rec.Reason = strings.TrimSpace(rec.Reason)
		rec.Actor = strings.TrimSpace(rec.Actor)
		if err := rec.validate(); err != nil {
			return nil, 0, fmt.Errorf("regime backfill: %w", err)
		}
		key := regimeRecordKeyOf(rec)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		line, err := json.Marshal(rec)
		if err != nil {
			return nil, 0, fmt.Errorf("regime backfill: encoding: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
		n++
	}
	return buf.Bytes(), n, nil
}

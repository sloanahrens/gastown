package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// The install receipts: one JSON line per installer run, written under the
// town's daemon directory by the gt installer (internal/installer) and the om
// installer (internal/ominstaller). RecordInstallReceipts keeps the regime log
// current from them: each receipt becomes one install regime event, so an
// install is recorded without a person typing it, and a second run records
// nothing because the event's (kind, ts, after_ref) is already in the log.
//
// A receipt is untrusted input. A file or line the reader cannot use is
// reported on the log it is given and skipped, and the other receipts still
// record; a missing file is no receipt, which is not an error. The runtime
// record is read first, so a record that cannot be read fails the run rather
// than replaying every receipt into it.
//
// A receipt records a run, not necessarily a change: a refused or failed run
// carries the commit it tried to install rather than one it put in force. It is
// still recorded, because every receipt is the run's own record and the event's
// reason names what happened.

// installReceiptsFiles are the receipts files, relative to the town root, in
// the order they are read. Both installers write the same line shape.
var installReceiptsFiles = []string{
	filepath.Join("daemon", "install-receipts.jsonl"),
	filepath.Join("daemon", "install-om-receipts.jsonl"),
}

// installReceipt is the part of one receipt line a regime event is made from.
// A field the run had nothing to say about is null, which decodes as "".
type installReceipt struct {
	TS         string `json:"ts"`
	Event      string `json:"event"`
	Commit     string `json:"commit"`
	PrevCommit string `json:"prev_commit"`
	Reason     string `json:"reason"`
}

// ReceiptsResult is what one sync did: how many receipts became new events, and
// how many the log already held.
type ReceiptsResult struct {
	Recorded int
	Skipped  int
}

// regimeRecordKey is what makes two regime records the same event to the
// reader: the kind, the instant and the ref after the change.
type regimeRecordKey struct {
	kind     string
	afterRef string
	ts       time.Time
}

// regimeRecordKeyOf is the record's identity under the reader's duplicate rule.
func regimeRecordKeyOf(rec RegimeEventRecord) regimeRecordKey {
	return regimeRecordKey{kind: rec.Kind, afterRef: rec.AfterRef, ts: rec.TS.UTC()}
}

// RecordInstallReceipts appends one install regime event for each receipt the
// town's regime log does not already hold, and returns what it did. logLine is
// told one line per receipt file or line it cannot use; the other receipts
// still record. A record the writer cannot append ends the run with the error,
// and the receipts it already wrote keep their lines: the record is
// append-only, so a later run picks up where this one stopped.
func RecordInstallReceipts(env Env, actor string, logLine func(string)) (ReceiptsResult, error) {
	var res ReceiptsResult
	if env.ReadFile == nil {
		return res, errors.New("install receipts: Env.ReadFile is unset")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return res, errors.New("install receipts: no actor to record the events under")
	}
	if logLine == nil {
		logLine = func(string) {}
	}
	path := RegimeEventsPath(env.Town)
	seen, err := recordedEventKeys(env.ReadFile, path)
	if err != nil {
		return res, err
	}
	for _, rel := range installReceiptsFiles {
		data, err := env.ReadFile(filepath.Join(env.Town, rel))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			logLine(fmt.Sprintf("install receipts: %s: %v", rel, err))
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			rec, err := receiptRegimeEvent(line, rel, actor)
			if err != nil {
				logLine(fmt.Sprintf("install receipts: %s line %d: %v", rel, i+1, err))
				continue
			}
			key := regimeRecordKeyOf(rec)
			if _, dup := seen[key]; dup {
				res.Skipped++
				continue
			}
			seen[key] = struct{}{}
			if err := RecordRegimeEvent(path, rec); err != nil {
				return res, err
			}
			res.Recorded++
		}
	}
	return res, nil
}

// recordedEventKeys reads the runtime record and returns the keys of the
// events it already holds. A missing record holds nothing; one that cannot be
// read or decoded is an error, never an empty record, because replaying every
// receipt into a log the run could not read is not a clean second run.
func recordedEventKeys(read func(string) ([]byte, error), path string) (map[regimeRecordKey]struct{}, error) {
	seen := make(map[regimeRecordKey]struct{})
	data, err := read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return seen, nil
	}
	if err != nil {
		return nil, fmt.Errorf("install receipts: reading %s: %w", path, err)
	}
	records, err := parseRegimeRecords(data)
	if err != nil {
		return nil, fmt.Errorf("install receipts: %s: %w", path, err)
	}
	for _, rec := range records {
		seen[regimeRecordKeyOf(rec)] = struct{}{}
	}
	return seen, nil
}

// receiptRegimeEvent builds the install regime event for one receipt line. The
// commit the receipt names after the run is the event's ref after the change,
// and the one before it, the ref before.
func receiptRegimeEvent(line, source, actor string) (RegimeEventRecord, error) {
	var r installReceipt
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return RegimeEventRecord{}, fmt.Errorf("decoding: %w", err)
	}
	event := strings.TrimSpace(r.Event)
	if event == "" {
		return RegimeEventRecord{}, errors.New("no event")
	}
	if strings.TrimSpace(r.TS) == "" {
		return RegimeEventRecord{}, errors.New("no timestamp")
	}
	ts, err := ParseTS(r.TS)
	if err != nil {
		return RegimeEventRecord{}, err
	}
	commit := strings.TrimSpace(r.Commit)
	return RegimeEventRecord{
		TS:        ts.UTC(),
		Kind:      "install",
		BeforeRef: strings.TrimSpace(r.PrevCommit),
		AfterRef:  commit,
		Commit:    commit,
		Reason:    receiptReason(event, r),
		Actor:     actor,
		Source:    filepath.ToSlash(source),
	}, nil
}

// receiptReason is the one line the event carries: what the run did, the commit
// it did it with, and the receipt's own reason when it has one.
func receiptReason(event string, r installReceipt) string {
	reason := event
	if commit := strings.TrimSpace(r.Commit); commit != "" {
		reason += " " + commit
	}
	if why := strings.TrimSpace(r.Reason); why != "" {
		reason += " (" + why + ")"
	}
	return reason
}

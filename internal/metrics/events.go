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

// The town's regime events: the days the regime changed (seat models, gate
// thresholds), read from two places. The operator writes
// <town>/settings/regime-events.json by hand; gt regime record appends to
// <town>/.runtime/metrics/regime-events.jsonl. The panel marks the weeks an
// event names, so both decodes are strict and a hand-edit that misspells a key
// fails here rather than shading a week.
//
// The settings file is schema version 1:
//
//	{"v": 1, "events": [{"date": "2026-10-10", "what": "flash seats replaced by Sonnet seats"}]}
//
// A MISSING settings file is not an error: no event is known yet, and the caller
// gets an empty list it can marshal as []. An entry may also carry the
// RegimeEvent's "confirmed", which the backtest reads; any other field is an
// unknown key.
//
// The runtime record is append-only, one RegimeEventRecord per line (see its
// comment). Beside it the operator writes regime-events-backfill-<day>.jsonl for
// the changes known before the record existed: the same shape plus "source", the
// record the change was read from. The reader merges all three, ordered by the
// instant each event happened at, with a settings event placed at its day's UTC
// midnight. A record repeated across the runtime files (same kind, ts and
// after_ref) counts once, at the first occurrence; the main file is read before
// the backfill files, which are read in name order. A record whose after_ref
// differs from its twin is a different event to this rule and is marked twice.
//
// The events come back sorted oldest first, and two events at one instant keep
// their source order, so one week's marks read in the order the sources wrote
// them.

// regimeEventsFile is the settings file's path under the town root.
var regimeEventsFile = filepath.Join("settings", "regime-events.json")

// The runtime record's location under the town root, and the names read there.
var regimeEventsRuntimeDir = filepath.Join(".runtime", "metrics")

const (
	regimeEventsJSONLName      = "regime-events.jsonl"
	regimeEventsBackfillPrefix = "regime-events-backfill-"
	regimeEventsBackfillSuffix = ".jsonl"
)

// RegimeKinds are the kinds a regime event may be recorded as: what changed.
// The writer refuses any other and the reader refuses a line that carries one.
var RegimeKinds = []string{
	"install", "directive", "gate", "seat_model", "pool", "dispatch_hold",
	"ring", "tracker", "platform", "dolt_flatten", "policy", "other",
}

// regimeKindValid reports whether kind is one of RegimeKinds.
func regimeKindValid(kind string) bool {
	for _, known := range RegimeKinds {
		if kind == known {
			return true
		}
	}
	return false
}

// RegimeEventsPath is the runtime record gt regime record appends to for town:
// the file ReadRegimeEvents merges with settings/regime-events.json.
func RegimeEventsPath(town string) string {
	return filepath.Join(town, regimeEventsRuntimeDir, regimeEventsJSONLName)
}

// regimeEventsVersion is the only settings schema version this reader accepts.
const regimeEventsVersion = 1

// regimeEventsMax bounds the settings file so a hand-written list cannot grow
// the panel without limit. The runtime record is append-only and unbounded; the
// panel keeps its newest panelEventsMax marks.
const regimeEventsMax = 64

// regimeEventWhatMax is the longest "what" a panel line carries, in runes.
const regimeEventWhatMax = 120

// regimeEventsDateLayout is the only form a date may take.
const regimeEventsDateLayout = "2006-01-02"

// regimeEventsDoc is the file's shape.
type regimeEventsDoc struct {
	V      int           `json:"v"`
	Events []RegimeEvent `json:"events"`
}

// RegimeEventRecord is one line of the runtime record: the instant a regime
// changed, what changed, the refs either side of it, and who recorded it. Source
// is set only on a backfilled line, which names the record the change was read
// from.
type RegimeEventRecord struct {
	TS        time.Time `json:"ts"`
	Kind      string    `json:"kind"`
	BeforeRef string    `json:"before_ref"`
	AfterRef  string    `json:"after_ref"`
	Commit    string    `json:"commit"`
	Reason    string    `json:"reason"`
	Actor     string    `json:"actor"`
	Source    string    `json:"source,omitempty"`
}

// event is the record as the reader reports it: the UTC day it happened on and
// what changed. A recorded event is confirmed, because it was written with its
// actor and its refs when the change happened rather than remembered by hand.
func (r RegimeEventRecord) event() RegimeEvent {
	what := r.Kind + ": " + strings.TrimSpace(r.Reason)
	if runes := []rune(what); len(runes) > regimeEventWhatMax {
		what = string(runes[:regimeEventWhatMax])
	}
	return RegimeEvent{
		Date:      r.TS.UTC().Format(regimeEventsDateLayout),
		What:      what,
		Confirmed: true,
	}
}

// validate refuses a record the reader could not use: it must name a known kind,
// an instant, a reason and an actor. The writer and the reader share it, so a
// line the reader would reject is never one the writer produced.
func (r RegimeEventRecord) validate() error {
	switch {
	case r.TS.IsZero():
		return errors.New("no timestamp")
	case !regimeKindValid(r.Kind):
		return fmt.Errorf("unknown kind %q, want one of %s", r.Kind, strings.Join(RegimeKinds, ", "))
	case strings.TrimSpace(r.Reason) == "":
		return errors.New("empty reason")
	case strings.TrimSpace(r.Actor) == "":
		return errors.New("empty actor")
	}
	return nil
}

// regimeFile is what the append needs of the file it writes: one write and a
// close.
type regimeFile interface {
	Write([]byte) (int, error)
	Close() error
}

// regimeAppend is the record's way to the file: it encodes the line and opens
// the file. The production values are json.Marshal and os.OpenFile. A record's
// line is strings and a timestamp, so neither fails on a healthy host; the
// struct exists so a test drives the encoding, write, short-write and close
// failures a host cannot be made to produce.
type regimeAppend struct {
	encode func(any) ([]byte, error)
	open   func(name string, flag int, perm os.FileMode) (regimeFile, error)
}

// hostRegimeAppend is the process's own encoder and filesystem.
func hostRegimeAppend() regimeAppend {
	return regimeAppend{encode: json.Marshal, open: openRegimeFile}
}

// openRegimeFile opens the record file for appending.
func openRegimeFile(name string, flag int, perm os.FileMode) (regimeFile, error) {
	return os.OpenFile(name, flag, perm)
}

// RecordRegimeEvent appends ev to the regime events file at path as one line,
// creating the file 0600 and its directory when they are missing. The append is
// one O_APPEND write of one line, so a concurrent record cannot interleave with
// it. An unknown kind, an empty reason, an empty actor or a zero timestamp is
// refused and nothing is written.
func RecordRegimeEvent(path string, ev RegimeEventRecord) error {
	return appendRegimeEvent(path, ev, hostRegimeAppend())
}

// appendRegimeEvent is RecordRegimeEvent through seams. A failed write still
// closes the file, and a close failure is an error so a record the host did not
// keep is not reported as written.
func appendRegimeEvent(path string, ev RegimeEventRecord, seams regimeAppend) error {
	ev.Kind = strings.TrimSpace(ev.Kind)
	ev.Reason = strings.TrimSpace(ev.Reason)
	ev.Actor = strings.TrimSpace(ev.Actor)
	if err := ev.validate(); err != nil {
		return fmt.Errorf("regime event: %w", err)
	}
	line, err := seams.encode(ev)
	if err != nil {
		return fmt.Errorf("regime event: encoding: %w", err)
	}
	line = append(line, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("regime event: creating %s: %w", dir, err)
	}
	f, err := seams.open(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("regime event: opening %s: %w", path, err)
	}
	n, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("regime event: appending to %s: %w", path, werr)
	}
	if n != len(line) {
		return fmt.Errorf("regime event: appending to %s: %w", path, io.ErrShortWrite)
	}
	if cerr != nil {
		return fmt.Errorf("regime event: closing %s: %w", path, cerr)
	}
	return nil
}

// regimeEventOn is an event and the instant that orders it against the events
// from the other source. A settings event is its day's UTC midnight.
type regimeEventOn struct {
	at    time.Time
	event RegimeEvent
}

// ReadRegimeEvents reads the town's regime events from the settings file and the
// runtime record, merged oldest first. A missing file or runtime directory means
// no events are known there, and returns an empty, non-nil list rather than an
// error; a file that is there and cannot be read or decoded is an error.
func ReadRegimeEvents(env Env) ([]RegimeEvent, error) {
	settings, err := readSettingsRegimeEvents(env)
	if err != nil {
		return nil, err
	}
	records, err := readRegimeRecords(env)
	if err != nil {
		return nil, err
	}
	return mergeRegimeEvents(settings, records), nil
}

// readSettingsRegimeEvents reads and decodes settings/regime-events.json.
func readSettingsRegimeEvents(env Env) ([]regimeEventOn, error) {
	data, err := env.ReadFile(filepath.Join(env.Town, regimeEventsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", regimeEventsFile, err)
	}
	events, err := parseRegimeEvents(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", regimeEventsFile, err)
	}
	return events, nil
}

// readRegimeRecords reads the runtime record and any backfill files beside it,
// in the order they are merged: the main file first, then the backfill files in
// the order the directory lists them. The directory is listed first, because a
// missing directory means there is no record to read. A file that is not there
// has no records; a file that is there and cannot be read or decoded, and a
// directory that cannot be listed, are errors rather than an empty record.
func readRegimeRecords(env Env) ([]RegimeEventRecord, error) {
	if env.ReadDir == nil {
		return nil, errors.New("regime events: Env.ReadDir is unset")
	}
	dir := filepath.Join(env.Town, regimeEventsRuntimeDir)
	entries, err := env.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", regimeEventsRuntimeDir, err)
	}
	names := []string{regimeEventsJSONLName}
	for _, entry := range entries {
		name := entry.Name
		if entry.IsDir || name == regimeEventsJSONLName {
			continue
		}
		if strings.HasPrefix(name, regimeEventsBackfillPrefix) && strings.HasSuffix(name, regimeEventsBackfillSuffix) {
			names = append(names, name)
		}
	}
	var records []RegimeEventRecord
	for _, name := range names {
		rel := filepath.Join(regimeEventsRuntimeDir, name)
		data, err := env.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", rel, err)
		}
		fileRecords, err := parseRegimeRecords(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		records = append(records, fileRecords...)
	}
	return records, nil
}

// parseRegimeRecords decodes a runtime record strictly: one JSON object per
// line, each a record with a known kind, a reason, an actor and a timestamp.
func parseRegimeRecords(data []byte) ([]RegimeEventRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var records []RegimeEventRecord
	for {
		var rec RegimeEventRecord
		err := dec.Decode(&rec)
		if errors.Is(err, io.EOF) {
			return records, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decoding record %d: %w", len(records)+1, err)
		}
		if err := rec.validate(); err != nil {
			return nil, fmt.Errorf("record %d: %w", len(records)+1, err)
		}
		records = append(records, rec)
	}
}

// mergeRegimeEvents merges the settings events and the runtime records into the
// reader's list: oldest first, a record repeated across the runtime files
// (same kind, ts and after_ref) counted once at its first occurrence, and a
// record rendered as the day it happened on and what changed.
func mergeRegimeEvents(settings []regimeEventOn, records []RegimeEventRecord) []RegimeEvent {
	placed := make([]regimeEventOn, 0, len(settings)+len(records))
	placed = append(placed, settings...)
	type recordKey struct {
		kind     string
		afterRef string
		ts       time.Time
	}
	seen := make(map[recordKey]struct{}, len(records))
	for _, rec := range records {
		key := recordKey{kind: rec.Kind, afterRef: rec.AfterRef, ts: rec.TS.UTC()}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		placed = append(placed, regimeEventOn{at: rec.TS.UTC(), event: rec.event()})
	}
	sort.SliceStable(placed, func(i, j int) bool { return placed[i].at.Before(placed[j].at) })
	events := make([]RegimeEvent, len(placed))
	for i, p := range placed {
		events[i] = p.event
	}
	return events
}

// parseRegimeEvents decodes a settings file strictly and returns its events,
// validated, trimmed and sorted by day.
func parseRegimeEvents(data []byte) ([]regimeEventOn, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc regimeEventsDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decoding: data after the top-level object")
	}
	if doc.V != regimeEventsVersion {
		return nil, fmt.Errorf("schema version %d, want %d", doc.V, regimeEventsVersion)
	}
	if len(doc.Events) > regimeEventsMax {
		return nil, fmt.Errorf("%d events, want at most %d", len(doc.Events), regimeEventsMax)
	}
	dated := make([]regimeEventOn, 0, len(doc.Events))
	for i, event := range doc.Events {
		day, err := time.Parse(regimeEventsDateLayout, event.Date)
		if err != nil {
			return nil, fmt.Errorf("event %d: date %q: %w", i+1, event.Date, err)
		}
		what := strings.TrimSpace(event.What)
		if what == "" {
			return nil, fmt.Errorf("event %d: empty \"what\"", i+1)
		}
		if runes := []rune(what); len(runes) > regimeEventWhatMax {
			what = string(runes[:regimeEventWhatMax])
		}
		dated = append(dated, regimeEventOn{
			event: RegimeEvent{Date: event.Date, What: what, Confirmed: event.Confirmed},
			at:    day,
		})
	}
	sort.SliceStable(dated, func(i, j int) bool { return dated[i].at.Before(dated[j].at) })
	return dated, nil
}

package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The town's regime events: the days the regime changed (seat models, gate
// thresholds), read from <town>/settings/regime-events.json. The operator writes
// the file and the panel marks the weeks an event names, so the decode is strict
// and a hand-edit that misspells a key fails here rather than shading a week.
// Schema version 1:
//
//	{"v": 1, "events": [{"date": "2026-10-10", "what": "flash seats replaced by Sonnet seats"}]}
//
// A MISSING file is not an error: no event is known yet, and the caller gets an
// empty list it can marshal as []. An entry may also carry the RegimeEvent's
// "confirmed", which the backtest reads; any other field is an unknown key.
//
// The events come back sorted by date, and two events on one day keep their
// file order, so one week's marks read in the order the file wrote them.

// regimeEventsFile is the file's path under the town root.
var regimeEventsFile = filepath.Join("settings", "regime-events.json")

// regimeEventsVersion is the only schema version this reader accepts.
const regimeEventsVersion = 1

// regimeEventsMax bounds a file so a runaway writer cannot grow the panel
// without limit.
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

// ReadRegimeEvents reads the town's regime events; a missing file means none
// are known, and returns an empty, non-nil list rather than an error.
func ReadRegimeEvents(env Env) ([]RegimeEvent, error) {
	data, err := env.ReadFile(filepath.Join(env.Town, regimeEventsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return []RegimeEvent{}, nil
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

// parseRegimeEvents decodes a regime-events file strictly and returns its
// events, validated, trimmed and sorted.
func parseRegimeEvents(data []byte) ([]RegimeEvent, error) {
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
	type datedEvent struct {
		event RegimeEvent
		day   time.Time
	}
	dated := make([]datedEvent, 0, len(doc.Events))
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
		dated = append(dated, datedEvent{
			event: RegimeEvent{Date: event.Date, What: what, Confirmed: event.Confirmed},
			day:   day,
		})
	}
	sort.SliceStable(dated, func(i, j int) bool { return dated[i].day.Before(dated[j].day) })
	events := make([]RegimeEvent, len(dated))
	for i, d := range dated {
		events[i] = d.event
	}
	return events, nil
}

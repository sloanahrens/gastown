// Package metrics holds the town's hourly snapshot in Go: the record every
// reader of snapshots.jsonl parses, the sources that fill it, and Collect; and
// the join that puts the snapshot in force beside every landing in
// landings.csv.
//
// A record is one JSON line: the fixed keys v, ts, placement and errors, then
// one top-level key per source group. Keys and columns are additive within
// Version and a column is never renamed, so a reader of an older record keeps
// working on a newer one (gt-pkrmg).
package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Version is the schema version written as v. A reader keys its parsing on it.
const Version = 1

// PlacementGroup is the group whose columns fill the record's fixed placement
// key rather than a group key of their own: where each component runs is part
// of reading every other group.
const PlacementGroup = "placement"

// Record is one snapshot line: the fixed keys v, ts, placement and errors, and
// one top-level key per source group.
type Record struct {
	// V is the schema version, Version.
	V int
	// TS is the time the snapshot was taken, in RFC 3339 UTC.
	TS string
	// Placement maps each component to the place it runs, the value of the
	// fixed placement key. nil marshals as null, meaning no placement is known.
	Placement map[string]any
	// Groups holds each source's columns under its group name. A group whose
	// source failed is present with a nil map, which marshals as null.
	Groups map[string]map[string]any
	// Errors names each group Collect could not fill, in source order.
	Errors []Error
}

// Error is one source a record could not collect: the group it left null and
// why.
type Error struct {
	Group string `json:"group"`
	Error string `json:"error"`
}

// ReadSnapshots reads one record per line, in file order. The log is appended
// to while this reads, so an unfinished last line is skipped; a line that is
// not a record anywhere else in the file is an error, because then the file is
// not the snapshot log. A blank line names no record and is skipped.
func ReadSnapshots(r io.Reader) ([]Record, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("snapshots: %w", err)
	}
	text := string(data)
	lines := strings.Split(text, "\n")
	records := make([]Record, 0, len(lines))
	for i, line := range lines {
		last := i == len(lines)-1
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			if last && !strings.HasSuffix(text, "\n") {
				break // a record an append is still writing
			}
			return nil, fmt.Errorf("snapshot line %d: %w", i+1, err)
		}
		records = append(records, rec)
	}
	return records, nil
}

// fixedKey reports whether name is one of the record's own keys, which a
// source may not claim as its group.
func fixedKey(name string) bool {
	switch name {
	case "v", "ts", PlacementGroup, "errors":
		return true
	}
	return false
}

// MarshalJSON writes one flat object: the fixed keys and one key per group.
// A group named a fixed key is left out, because the fixed key is the record's
// own and the group's columns have nowhere to go. The keys come out in the
// order json.Marshal gives a map, so the bytes are stable and the golden
// fixture can pin them.
func (r Record) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(r.Groups)+4)
	out["v"] = r.V
	out["ts"] = r.TS
	out["placement"] = r.Placement
	errs := r.Errors
	if errs == nil {
		errs = []Error{}
	}
	out["errors"] = errs
	for name, cols := range r.Groups {
		if fixedKey(name) {
			continue
		}
		out[name] = cols
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads a record MarshalJSON wrote. A key that is not fixed is a
// group, and an unknown key is carried rather than refused: the record is
// additive within Version.
func (r *Record) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("snapshot record: %w", err)
	}
	*r = Record{}
	for name, raw := range fields {
		var err error
		switch name {
		case "v":
			err = json.Unmarshal(raw, &r.V)
		case "ts":
			err = json.Unmarshal(raw, &r.TS)
		case PlacementGroup:
			err = json.Unmarshal(raw, &r.Placement)
		case "errors":
			err = json.Unmarshal(raw, &r.Errors)
		default:
			var cols map[string]any
			if err := json.Unmarshal(raw, &cols); err != nil {
				return fmt.Errorf("snapshot group %q: %w", name, err)
			}
			if r.Groups == nil {
				r.Groups = make(map[string]map[string]any, len(fields))
			}
			r.Groups[name] = cols
			continue
		}
		if err != nil {
			return fmt.Errorf("snapshot %s: %w", name, err)
		}
	}
	return nil
}

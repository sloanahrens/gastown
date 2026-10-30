package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// createdBeadID is what a golden calls a bead a row created. bd mints that id from the run, so the
// committed golden holds this in its place (gt-p85pw.103).
const createdBeadID = "fx-new"

// fixedTimestamp is what a golden holds wherever bd writes the time a bead was created, changed or
// closed, so a golden does not change when the recording does.
const fixedTimestamp = "2026-01-01T00:00:00Z"

// timestampFields are the fields bd fills with the time a bead was written.
var timestampFields = map[string]bool{
	"created_at": true,
	"updated_at": true,
	"closed_at":  true,
}

// normalizeGolden rewrites one row's JSON stdout into the form the committed goldens hold: the id bd
// minted for a bead the row created becomes createdBeadID, every RFC 3339 timestamp under
// created_at, updated_at or closed_at becomes fixedTimestamp, and every revision number becomes 0.
// An id the fixture itself holds, which keep names, is left as it is, so a fixture bead is never
// mistaken for a bead the run created. Output that is not one JSON value is returned as bd printed
// it, and so is JSON in which nothing was rewritten, so a stable golden is still exactly bd's bytes.
func normalizeGolden(stdout []byte, keep map[string]bool) []byte {
	value, ok := decodeJSON(stdout)
	if !ok {
		return stdout
	}
	normalized, changed := normalizeValue(value, keep)
	if !changed {
		return stdout
	}
	out, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return stdout
	}
	return append(out, '\n')
}

// decodeJSON returns the one JSON value stdout holds, and whether it holds exactly one.
func decodeJSON(stdout []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.UseNumber() // a number keeps its digits, so a golden holds what bd printed
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false // anything after the first value means this is not one JSON answer
	}
	return value, true
}

// normalizeValue rewrites the values bd decides at run time inside one decoded JSON value, and
// reports whether it changed any.
func normalizeValue(value any, keep map[string]bool) (any, bool) {
	switch v := value.(type) {
	case map[string]any:
		changed := false
		for key, field := range v {
			if rewritten, ok := normalizeField(key, field); ok {
				v[key] = rewritten
				changed = true
				continue
			}
			if rewritten, ok := normalizeValue(field, keep); ok {
				v[key] = rewritten
				changed = true
			}
		}
		return v, changed
	case []any:
		changed := false
		for i, item := range v {
			if rewritten, ok := normalizeValue(item, keep); ok {
				v[i] = rewritten
				changed = true
			}
		}
		return v, changed
	case string:
		if isCreatedBeadID(v, keep) {
			return createdBeadID, true
		}
	}
	return value, false
}

// normalizeField rewrites the one value under key that bd fills from the run rather than the
// fixture, and reports whether it did.
func normalizeField(key string, value any) (any, bool) {
	if timestampFields[key] {
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		if _, err := time.Parse(time.RFC3339Nano, text); err != nil {
			return nil, false
		}
		return fixedTimestamp, true
	}
	if key == "revision" {
		if _, ok := value.(json.Number); !ok {
			return nil, false
		}
		return json.Number("0"), true
	}
	return nil, false
}

// isCreatedBeadID reports whether s is an id bd minted for a bead a row created: one carrying the
// fixture's prefix that the fixture file does not hold itself.
func isCreatedBeadID(s string, keep map[string]bool) bool {
	return strings.HasPrefix(s, fixturePrefix+"-") && !keep[s]
}

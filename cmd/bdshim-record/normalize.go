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

// beadIDFields are the fields bd writes a bead's id under. Only a value here is replaced by
// createdBeadID: a title or a description that happens to begin with the fixture prefix is fixture
// text, and rewriting it would hide a difference a golden exists to show (gt-p85pw.103).
var beadIDFields = map[string]bool{
	"id":            true,
	"issue_id":      true,
	"depends_on_id": true,
	"bead_id":       true,
	"parent_id":     true,
	"child_id":      true,
	"parent":        true,
	"blocked_by":    true,
	"blocks":        true,
	"ids":           true,
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
	normalized, changed := normalizeValue(value, keep, "")
	if !changed {
		return stdout
	}
	return append(appendJSONValue(nil, normalized, ""), '\n')
}

// jsonMember is one member of a JSON object, kept in the order bd printed it.
type jsonMember struct {
	key   string
	value any
}

// jsonObject is a decoded JSON object in bd's member order. A Go map would sort the keys on the way
// out, so the golden would no longer read the way bd answers (gt-p85pw.103).
type jsonObject []jsonMember

// decodeJSON returns the one JSON value stdout holds, and whether it holds exactly one.
func decodeJSON(stdout []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.UseNumber() // a number keeps its digits, so a golden holds what bd printed
	value, ok := decodeOrdered(decoder)
	if !ok {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false // anything after the first value means this is not one JSON answer
	}
	return value, true
}

// decodeOrdered reads the next JSON value and reports whether it read one, so the one caller that
// decides between bd's bytes and a normalized golden hears "not JSON" rather than a syntax error.
// Objects become jsonObject, arrays become []any, and every scalar is the decoder's own value, so a
// number keeps its digits.
func decodeOrdered(decoder *json.Decoder) (any, bool) {
	token, err := decoder.Token()
	if err != nil {
		return nil, false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, true
	}
	switch delim {
	case '{':
		object := jsonObject{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, false
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, false
			}
			value, ok := decodeOrdered(decoder)
			if !ok {
				return nil, false
			}
			object = append(object, jsonMember{key: key, value: value})
		}
		if _, err := decoder.Token(); err != nil { // the closing brace
			return nil, false
		}
		return object, true
	case '[':
		array := []any{}
		for decoder.More() {
			item, ok := decodeOrdered(decoder)
			if !ok {
				return nil, false
			}
			array = append(array, item)
		}
		if _, err := decoder.Token(); err != nil { // the closing bracket
			return nil, false
		}
		return array, true
	}
	return nil, false
}

// normalizeValue rewrites the values bd decides at run time inside one decoded JSON value, and
// reports whether it changed any. key is the member name value was printed under, so a bead id is
// told apart from fixture text that reads the same.
func normalizeValue(value any, keep map[string]bool, key string) (any, bool) {
	switch v := value.(type) {
	case jsonObject:
		changed := false
		for i, member := range v {
			if rewritten, ok := normalizeField(member.key, member.value); ok {
				v[i].value = rewritten
				changed = true
				continue
			}
			if rewritten, ok := normalizeValue(member.value, keep, member.key); ok {
				v[i].value = rewritten
				changed = true
			}
		}
		return v, changed
	case []any:
		changed := false
		for i, item := range v {
			if rewritten, ok := normalizeValue(item, keep, key); ok {
				v[i] = rewritten
				changed = true
			}
		}
		return v, changed
	case string:
		if beadIDFields[key] && isCreatedBeadID(v, keep) {
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

// appendJSONValue writes value as JSON indented by two spaces per level, in the order it was
// decoded, and leaves <, > and & as bd printed them. Encoding/json's own marshal would sort the
// keys and escape those characters, so the golden would stop being bd's answer (gt-p85pw.103).
func appendJSONValue(dst []byte, value any, indent string) []byte {
	inner := indent + "  "
	switch v := value.(type) {
	case jsonObject:
		if len(v) == 0 {
			return append(dst, "{}"...)
		}
		dst = append(dst, '{')
		for i, member := range v {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, '\n')
			dst = append(dst, inner...)
			dst = appendJSONString(dst, member.key)
			dst = append(dst, ':', ' ')
			dst = appendJSONValue(dst, member.value, inner)
		}
		dst = append(dst, '\n')
		dst = append(dst, indent...)
		return append(dst, '}')
	case []any:
		if len(v) == 0 {
			return append(dst, "[]"...)
		}
		dst = append(dst, '[')
		for i, item := range v {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, '\n')
			dst = append(dst, inner...)
			dst = appendJSONValue(dst, item, inner)
		}
		dst = append(dst, '\n')
		dst = append(dst, indent...)
		return append(dst, ']')
	case string:
		return appendJSONString(dst, v)
	case json.Number:
		return append(dst, v.String()...)
	case bool:
		if v {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case nil:
		return append(dst, "null"...)
	}
	return dst
}

// appendJSONString appends s as a JSON string without HTML escaping, so a title bd printed with a
// literal & or < keeps it.
func appendJSONString(dst []byte, s string) []byte {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(s); err != nil { // a string always encodes; the bytes are the answer
		return dst
	}
	return append(dst, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
}

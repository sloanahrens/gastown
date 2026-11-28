package forgejoserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// Differences lists every difference between a rendered manifest object and a
// live one, in both directions: a value that differs, and a key or list entry
// that only one side holds. Both objects are decoded through JSON and stripped
// of the fields the cluster sets itself, so a server-set field is never a
// difference. Each line reads "<path>: rendered <x>, observed <y>", where x and
// y are "sha256:" and the first 12 hex digits of the value's canonical JSON, or
// "absent". A path names keys and list positions, never values. Lists compare by
// position, so an entry inserted mid-list reads as a difference at every later
// position. The lines are sorted. An object that is empty before or after
// stripping, or that does not encode as JSON, is an error: two empty sides would
// otherwise read as no difference.
func Differences(manifest, live map[string]any) ([]string, error) {
	rendered, err := normalizeSide("manifest", manifest)
	if err != nil {
		return nil, err
	}
	observed, err := normalizeSide("live", live)
	if err != nil {
		return nil, err
	}
	var lines []string
	walk(&lines, "", rendered, true, observed, true)
	sort.Strings(lines)
	return lines, nil
}

// normalizeSide decodes obj through JSON, so every value is a JSON kind whatever
// Go type the caller used, then strips the server-set fields. Numbers keep their
// text, so two different integers never compare equal.
func normalizeSide(side string, obj map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(obj)
	var decoded map[string]any
	if err == nil {
		decoded, err = decodeNumbers(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("%s object does not encode as JSON: %w", side, err)
	}
	stripped := Strip(decoded)
	if len(stripped) == 0 {
		return nil, fmt.Errorf("%s object has no fields to compare once the server-set fields are stripped", side)
	}
	return stripped, nil
}

// decodeNumbers reads encoder output back into JSON kinds, numbers as text. A
// side the JSON decoder refuses comes back as an error, never a panic.
func decodeNumbers(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded map[string]any
	if err := dec.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// walk appends one line for each path at which the two sides differ. A side that
// does not hold the path has its ok flag false, and the whole subtree on the other
// side is one line. Maps are walked key by key, lists by position, and anything
// else compares as one value.
func walk(lines *[]string, path string, rendered any, renderedOK bool, observed any, observedOK bool) {
	if !renderedOK || !observedOK {
		report(lines, path, rendered, renderedOK, observed, observedOK)
		return
	}
	if r, ok := rendered.(map[string]any); ok {
		if o, ok := observed.(map[string]any); ok {
			for _, k := range unionKeys(r, o) {
				rv, rok := r[k]
				ov, ook := o[k]
				walk(lines, joinKey(path, k), rv, rok, ov, ook)
			}
			return
		}
	}
	if r, ok := rendered.([]any); ok {
		if o, ok := observed.([]any); ok {
			for i := range max(len(r), len(o)) {
				var rv, ov any
				if i < len(r) {
					rv = r[i]
				}
				if i < len(o) {
					ov = o[i]
				}
				walk(lines, fmt.Sprintf("%s[%d]", path, i), rv, i < len(r), ov, i < len(o))
			}
			return
		}
	}
	if reflect.DeepEqual(rendered, observed) {
		return
	}
	report(lines, path, rendered, true, observed, true)
}

// report appends the line for one differing path.
func report(lines *[]string, path string, rendered any, renderedOK bool, observed any, observedOK bool) {
	*lines = append(*lines, fmt.Sprintf("%s: rendered %s, observed %s",
		path, describe(rendered, renderedOK), describe(observed, observedOK)))
}

// describe is the form a line carries for one side: "absent", or a digest of the
// value's canonical JSON. The value itself never appears.
func describe(v any, ok bool) string {
	if !ok {
		return "absent"
	}
	sum := sha256.Sum256(canonical(v))
	return "sha256:" + hex.EncodeToString(sum[:6])
}

// canonical is the JSON a value encodes to. Values reaching it came out of
// normalizeSide's decode, so they always encode; a failure is a bug in this file.
func canonical(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("decoded JSON value does not encode: %v", err))
	}
	return raw
}

// unionKeys returns the keys of either map, sorted.
func unionKeys(a, b map[string]any) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var keys []string
	for _, m := range []map[string]any{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func joinKey(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

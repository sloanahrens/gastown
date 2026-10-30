package scrub

import (
	"bytes"
	"encoding/json"
	"io"
)

// ScrubJSON returns a JSON document with every string value scrubbed at any
// depth, leaving keys, numbers and structure alone.
//
// A value under a credential-named key is replaced whole whatever its type — a
// string, an object, an array or a number — the way the assignment rule
// replaces NAME=value in text: the name stays, and it is what tells the
// scrubber the value hides a secret. Input that is not a JSON document is
// scrubbed as text, so nothing leaves unscrubbed.
func (s *Scrubber) ScrubJSON(raw []byte) ([]byte, Report) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return s.scrubRawJSON(raw)
	}
	if _, err := dec.Token(); err != io.EOF {
		// Trailing data means this is not one JSON document; re-encoding the
		// first value would silently drop the rest.
		return s.scrubRawJSON(raw)
	}
	doc, report := s.scrubJSONValue("", doc)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return s.scrubRawJSON(raw)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), report
}

// scrubRawJSON scrubs a document that is not JSON as text.
func (s *Scrubber) scrubRawJSON(raw []byte) ([]byte, Report) {
	text, report := s.Scrub(string(raw))
	return []byte(text), report
}

// scrubJSONValue scrubs one decoded value; key is the field holding it, empty
// inside an array or at the top level.
func (s *Scrubber) scrubJSONValue(key string, v any) (any, Report) {
	if credentialName(key) {
		// The whole value goes, not only its string leaves: a token object or
		// a secret list under a credential name is as much a secret as a
		// string, and a number beside the name is still the value it guards.
		return marker(RuleAssignment), Report{counts: map[string]int{RuleAssignment: 1}}
	}
	switch val := v.(type) {
	case string:
		text, report := s.Scrub(val)
		return text, report
	case map[string]any:
		report := Report{counts: make(map[string]int)}
		for k, child := range val {
			scrubbed, childReport := s.scrubJSONValue(k, child)
			val[k] = scrubbed
			report.add(childReport)
		}
		return val, report
	case []any:
		report := Report{counts: make(map[string]int)}
		for i, child := range val {
			scrubbed, childReport := s.scrubJSONValue("", child)
			val[i] = scrubbed
			report.add(childReport)
		}
		return val, report
	default:
		// Numbers, bools and null pass through untouched.
		return v, Report{}
	}
}

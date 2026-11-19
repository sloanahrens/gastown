// Package deployerinbox defines the request files agents write into the
// deployer's inbox and the answer files the deployer writes back (gt-r2nhus).
package deployerinbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

const (
	// MaxRequestBytes is the largest request file ParseRequest accepts; a
	// larger file is refused, never truncated.
	MaxRequestBytes = 4096

	// MaxDetailBytes is the largest Detail an answer carries; WriteAnswer
	// refuses a larger one rather than truncate it.
	MaxDetailBytes = 4096
)

// The kinds of work a request file asks the deployer for.
const (
	// KindPlan renders and hashes a plan, applying nothing.
	KindPlan = "plan"
	// KindApply applies a plan the operator approved.
	KindApply = "apply"
	// KindRollback undoes a plan to its recorded revisions.
	KindRollback = "rollback"
)

var (
	// planName is a plan directory name under the runner's state directory,
	// never a path.
	planName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

	// requestID is the request file's stem: a UTC second-resolution stamp and
	// eight hex digits.
	requestID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)
)

// requestKeys is the exact key set a request file may carry.
var requestKeys = map[string]bool{"id": true, "kind": true, "plan": true}

// Request is one file in the deployer's inbox: what to do and to which plan.
type Request struct {
	ID   string
	Kind string
	Plan string
}

// ParseRequest validates the request file named name and returns what it asks
// for, refusing any file whose shape differs from the schema (gt-r2nhus).
func ParseRequest(name string, data []byte) (Request, error) {
	if len(data) > MaxRequestBytes {
		return Request{}, fmt.Errorf("the request file %s is %d bytes, over the %d-byte limit", name, len(data), MaxRequestBytes)
	}

	fields, err := decodeRequestObject(data)
	if err != nil {
		return Request{}, fmt.Errorf("the request file %s %w", name, err)
	}
	id, err := textField(fields, "id")
	if err != nil {
		return Request{}, fmt.Errorf("the request file %s %w", name, err)
	}
	kind, err := textField(fields, "kind")
	if err != nil {
		return Request{}, fmt.Errorf("the request file %s %w", name, err)
	}
	plan, err := textField(fields, "plan")
	if err != nil {
		return Request{}, fmt.Errorf("the request file %s %w", name, err)
	}
	req := Request{ID: id, Kind: kind, Plan: plan}

	switch req.Kind {
	case KindPlan, KindApply, KindRollback:
	default:
		return Request{}, fmt.Errorf("the request file %s asks for the kind %q, not plan, apply or rollback", name, req.Kind)
	}
	if !planName.MatchString(req.Plan) {
		return Request{}, fmt.Errorf("the request file %s names the plan %q, which is not a plan directory name", name, req.Plan)
	}
	if !requestID.MatchString(req.ID) {
		return Request{}, fmt.Errorf("the request file %s has the id %q, not a UTC stamp and eight hex digits", name, req.ID)
	}
	if name != req.ID+".json" {
		return Request{}, fmt.Errorf("the request file %s carries the id %q, so its name is %s.json", name, req.ID, req.ID)
	}
	return req, nil
}

// decodeRequestObject decodes data as exactly one JSON object whose values are
// all strings. A map decode keeps the last value of a repeated key silently, so
// the object is read token by token and a key seen twice is refused.
func decodeRequestObject(data []byte) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("does not parse as JSON: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("is not a JSON object")
	}

	fields := make(map[string]string)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("does not parse as JSON: %w", err)
		}
		// Decoder.Token returns a string for every object key.
		key := keyTok.(string)
		if _, dup := fields[key]; dup {
			return nil, fmt.Errorf("has the key %q twice, and a request carries each key once", key)
		}
		if !requestKeys[key] {
			return nil, fmt.Errorf("has the key %q, and a request carries only id, kind and plan", key)
		}
		valueTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("does not parse as JSON: %w", err)
		}
		value, ok := valueTok.(string)
		if !ok {
			return nil, fmt.Errorf("has a %q value that is not a JSON string", key)
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("does not parse as JSON: %w", err)
	}
	if tok, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("does not parse as JSON: %w", err)
		}
		return nil, fmt.Errorf("has data after the request object (%v)", tok)
	}
	return fields, nil
}

// textField returns the string at key, refusing a missing key.
func textField(fields map[string]string, key string) (string, error) {
	value, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("has no %q field", key)
	}
	return value, nil
}

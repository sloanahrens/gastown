// Package deployerinbox defines the request files agents write into the
// deployer's inbox and the answer files the deployer writes back (gt-r2nhus).
package deployerinbox

import (
	"bytes"
	"encoding/json"
	"fmt"
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

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Request{}, fmt.Errorf("parse the request file %s: %w", name, err)
	}
	for key := range fields {
		if !requestKeys[key] {
			return Request{}, fmt.Errorf("the request file %s has the key %q, and a request carries only id, kind and plan", name, key)
		}
	}

	id, err := stringField(fields, "id")
	if err != nil {
		return Request{}, fmt.Errorf("the request file %s %w", name, err)
	}
	kind, err := stringField(fields, "kind")
	if err != nil {
		return Request{}, fmt.Errorf("the request file %s %w", name, err)
	}
	plan, err := stringField(fields, "plan")
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

// stringField returns the JSON string at key, refusing a missing key and a
// value that is not a JSON string, null included.
func stringField(fields map[string]json.RawMessage, key string) (string, error) {
	raw, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("has no %q field", key)
	}
	// A pointer stays nil for JSON null, which decoding into a string alone
	// would silently turn into the empty string.
	var value *string
	if err := json.Unmarshal(bytes.TrimSpace(raw), &value); err != nil || value == nil {
		return "", fmt.Errorf("has a %q value that is not a JSON string", key)
	}
	return *value, nil
}

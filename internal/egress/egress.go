// Package egress holds a workspace's egress allowlist as data: the hosts one
// workspace may reach, written per workspace so two workspaces differ without a
// code change.
//
// Nothing calls it yet. Its callers are the sandbox network setup and the
// gateway that enforces the policy, wired in a later bead; until then this
// package is the rule alone.
//
// The rule denies by default: a workspace with no list, or a host its list does
// not name, is refused. An entry is an exact host name or a leading "*."
// wildcard, and comparison ignores case and a :port suffix on either side.
package egress

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Policy is one workspace's egress allowlist, as Load reads it from data.
type Policy struct {
	// Workspace is the workspace the list belongs to. Load does not require
	// it: a list with no workspace is still a policy, and it still denies
	// every host it does not name.
	Workspace string `json:"workspace"`
	// Hosts are the reachable hosts: an exact name, or "*.example.com" for one
	// or more labels below example.com.
	Hosts []string `json:"hosts"`
}

// Load reads a policy from exactly one JSON object of the form
// {"workspace": "...", "hosts": ["..."]}, and refuses an unknown field, a
// second value or a malformed entry, because a typo read loosely would change
// which hosts the policy allows.
func Load(data []byte) (Policy, error) {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Policy{}, errors.New("egress policy: not a JSON object")
	}

	var p Policy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("egress policy: malformed JSON: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return Policy{}, errors.New("egress policy: unexpected data after the JSON object")
	}
	for i, host := range p.Hosts {
		if err := checkEntry(host); err != nil {
			return Policy{}, fmt.Errorf("egress policy: host entry %d: %w", i, err)
		}
	}
	return p, nil
}

// Allowed reports whether host is named by the policy: an empty policy or an
// unnamed host is refused, comparison ignores case and a :port suffix on either
// side, and a "*." entry matches one or more labels below its domain, never the
// domain itself.
func (p Policy) Allowed(host string) bool {
	name := normalizeHost(host)
	if name == "" {
		return false
	}
	for _, entry := range p.Hosts {
		// A Policy built by hand can hold an entry Load would refuse. Such an
		// entry is skipped rather than read loosely, so it can only deny.
		if checkEntry(entry) != nil {
			continue
		}
		if base, ok := strings.CutPrefix(entry, "*."); ok {
			base = normalizeHost(base)
			if len(name) > len(base)+1 && strings.HasSuffix(name, "."+base) {
				return true
			}
			continue
		}
		if normalizeHost(entry) == name {
			return true
		}
	}
	return false
}

// normalizeHost lower-cases a host and drops its :port suffix, so one host
// written two ways compares equal.
func normalizeHost(host string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return strings.ToLower(host)
}

// checkEntry refuses an entry that is not a host name, or a leading "*."
// wildcard, with an optional numeric port. It refuses rather than ignoring a
// malformed entry: an entry dropped silently is a host that quietly stops being
// reachable, and an entry read loosely (a URL, a glob anywhere but the front)
// could allow more than it says.
func checkEntry(entry string) error {
	name := entry
	if rest, ok := strings.CutPrefix(name, "*."); ok {
		name = rest
	} else if strings.Contains(name, "*") {
		return fmt.Errorf("%q: a wildcard must be the whole leading label", entry)
	}
	if name == "" {
		return errors.New("empty host entry")
	}
	if i := strings.IndexByte(name, ':'); i >= 0 {
		port := name[i+1:]
		name = name[:i]
		if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return fmt.Errorf("%q: bad port", entry)
		}
	}
	if name == "" {
		return errors.New("empty host entry")
	}
	// Each label must be letters, digits and hyphens. That also refuses a URL,
	// a path and a query, none of which is a host name.
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return fmt.Errorf("%q: empty label in host name", entry)
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return fmt.Errorf("%q: not a host name", entry)
			}
		}
	}
	return nil
}

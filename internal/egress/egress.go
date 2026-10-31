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

// Allowed reports whether host is named by the policy: an empty policy, a
// string that is not a host name, or an unnamed host is refused, comparison
// ignores case and a :port suffix on either side, and a "*." entry matches one
// or more labels below its domain, never the domain itself.
func (p Policy) Allowed(host string) bool {
	// A host argument that is not a host name cannot be named by any entry, so
	// it is refused rather than matched: a wildcard suffix would otherwise
	// accept a string such as "x/a.com" that no one can dial.
	if checkHostName(host) != nil {
		return false
	}
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
	if err := checkHostName(name); err != nil {
		return fmt.Errorf("%q: %w", entry, err)
	}
	return nil
}

// checkHostName refuses anything that is not a host name, optionally followed
// by a :port in 1..65535. It is the one validator for both a policy entry and
// the host Allowed is asked about, so the two cannot drift apart.
func checkHostName(s string) error {
	name, port, hasPort := splitHostPort(s)
	if hasPort {
		if err := checkPort(port); err != nil {
			return err
		}
	}
	if name == "" {
		return errors.New("empty host name")
	}
	// Each label must be letters, digits and hyphens. That also refuses a URL,
	// a path and a query, none of which is a host name.
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return errors.New("empty label in host name")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return errors.New("not a host name")
			}
		}
	}
	return nil
}

// splitHostPort cuts an optional :port suffix from s.
func splitHostPort(s string) (name, port string, hasPort bool) {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[:i], s[i+1:], true
	}
	return s, "", false
}

// checkPort refuses a port that is not a decimal number in 1..65535. An
// out-of-range port is an error for the same reason a bad label is: read
// loosely, it would be a port no one can dial.
func checkPort(port string) error {
	if port == "" {
		return errors.New("empty port")
	}
	n := 0
	for _, r := range port {
		if r < '0' || r > '9' {
			return errors.New("port is not a number")
		}
		n = n*10 + int(r-'0')
		if n > 65535 {
			return errors.New("port out of range")
		}
	}
	if n == 0 {
		return errors.New("port out of range")
	}
	return nil
}

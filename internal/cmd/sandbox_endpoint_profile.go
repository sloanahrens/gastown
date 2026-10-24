package cmd

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The forgejo-polecat provider profile side of `gt sandbox endpoint`
// (gt-ek39n): rendering the shipped profile for a gateway's endpoint, and
// judging whether the profile a gateway already holds binds that endpoint.

// profileDoc is the slice of a provider profile this reads. The endpoints share
// the policy file's shape, so a profile's endpoint decodes like a policy's.
type profileDoc struct {
	ResourceVersion int              `yaml:"resource_version"`
	Endpoints       []policyEndpoint `yaml:"endpoints"`
}

// renderForgejoProfile returns the forgejo-polecat provider profile src with its
// Forgejo endpoint (the one endpoint carrying the git smart-HTTP rules) bound to
// ep: the host and port values are replaced where they stand, and an
// allowed_ips line follows the port when ep has any. Every other byte of src is
// kept, so an endpoint equal to the one src names comes back identical and the
// Docker gateway imports the profile it always did. A src whose Forgejo endpoint
// already carries allowed_ips is refused: the shipped profile has none, and a
// second list would be a guess.
func renderForgejoProfile(src []byte, ep sandboxEndpoint) ([]byte, error) {
	var doc profileDoc
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("parsing the profile: %w", err)
	}
	idx := -1
	for i, e := range doc.Endpoints {
		if !hasGitSmartHTTPRule(e) {
			continue
		}
		if idx >= 0 {
			return nil, fmt.Errorf("the profile has more than one endpoint with the Forgejo git rules; want one")
		}
		idx = i
	}
	if idx < 0 {
		return nil, fmt.Errorf("the profile has no endpoint with the Forgejo git rules (a path ending .git/info/refs) to bind")
	}

	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return nil, fmt.Errorf("parsing the profile: %w", err)
	}
	endpoint := endpointNode(&root, idx)
	host, port := mapValue(endpoint, "host"), mapValue(endpoint, "port")
	portKey := mapKey(endpoint, "port")
	if host == nil || port == nil || portKey == nil {
		return nil, fmt.Errorf("the profile's Forgejo endpoint lacks a host or a port")
	}
	if mapValue(endpoint, "allowed_ips") != nil {
		return nil, fmt.Errorf("the profile's Forgejo endpoint already names allowed_ips; it would be bound twice")
	}
	if host.Line == port.Line {
		return nil, fmt.Errorf("the profile's Forgejo endpoint has its host and port on one line; give each its own line")
	}

	lines := strings.SplitAfter(string(src), "\n")
	var err error
	if lines[host.Line-1], err = replaceScalar(lines[host.Line-1], host, ep.Host); err != nil {
		return nil, err
	}
	if lines[port.Line-1], err = replaceScalar(lines[port.Line-1], port, strconv.Itoa(ep.Port)); err != nil {
		return nil, err
	}
	if len(ep.AllowedIPs) > 0 {
		eol := "\n"
		if !strings.HasSuffix(lines[port.Line-1], "\n") {
			eol = ""
			lines[port.Line-1] += "\n"
		}
		lines[port.Line-1] += strings.Repeat(" ", portKey.Column-1) + `allowed_ips: ["` + strings.Join(ep.AllowedIPs, `","`) + `"]` + eol
	}
	return []byte(strings.Join(lines, "")), nil
}

// stampResourceVersion returns profile with the resource_version the gateway's
// exported copy carries. The gateway refuses an update without one.
func stampResourceVersion(profile, exported []byte) ([]byte, error) {
	var doc profileDoc
	if err := yaml.Unmarshal(exported, &doc); err != nil {
		return nil, fmt.Errorf("parsing the exported profile: %w", err)
	}
	if doc.ResourceVersion < 1 {
		return nil, fmt.Errorf("the exported profile carries no resource_version, so it cannot be updated")
	}
	out := string(profile)
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return []byte(out + "resource_version: " + strconv.Itoa(doc.ResourceVersion) + "\n"), nil
}

// profileEndpointDiff compares the profile a gateway holds (as profile export
// prints it) with the endpoint it must bind. It returns "" when every endpoint
// of the profile names ep's host and port and the same set of allowed_ips, and
// otherwise a sentence saying what differs.
func profileEndpointDiff(exported []byte, ep sandboxEndpoint) (string, error) {
	var doc profileDoc
	if err := yaml.Unmarshal(exported, &doc); err != nil {
		return "", fmt.Errorf("parsing the exported profile: %w", err)
	}
	if len(doc.Endpoints) == 0 {
		return "the profile names no endpoint", nil
	}
	want := normalizedPrefixes(ep.AllowedIPs)
	for _, e := range doc.Endpoints {
		if e.Host != ep.Host || e.Port != ep.Port || !slices.Equal(normalizedPrefixes(e.AllowedIPs), want) {
			return fmt.Sprintf("the profile binds %s:%d allowed_ips [%s], want %s:%d allowed_ips [%s]",
				e.Host, e.Port, strings.Join(e.AllowedIPs, ","), ep.Host, ep.Port, strings.Join(ep.AllowedIPs, ",")), nil
		}
	}
	return "", nil
}

// normalizedPrefixes returns the sorted canonical form of CIDR strings; one that
// does not parse is kept as written, so it differs from every valid prefix.
func normalizedPrefixes(in []string) []string {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		if p, err := netip.ParsePrefix(raw); err == nil {
			raw = p.String()
		}
		out = append(out, raw)
	}
	slices.Sort(out)
	return out
}

// endpointNode returns the idx-th entry of the document's endpoints list.
func endpointNode(root *yaml.Node, idx int) *yaml.Node {
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil
	}
	seq := mapValue(root.Content[0], "endpoints")
	if seq == nil || seq.Kind != yaml.SequenceNode || idx >= len(seq.Content) {
		return nil
	}
	return seq.Content[idx]
}

// mapKey and mapValue look a key up in a mapping node; nil when absent.
func mapKey(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i]
		}
	}
	return nil
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// replaceScalar swaps the plain scalar node's text on its line for value,
// leaving the rest of the line (indent, key, trailing comment) as it was.
func replaceScalar(line string, node *yaml.Node, value string) (string, error) {
	start := node.Column - 1
	end := start + len(node.Value)
	if node.Style != 0 || start < 0 || end > len(line) || line[start:end] != node.Value {
		return "", fmt.Errorf("line %d: %q is not a plain value this can replace", node.Line, node.Value)
	}
	return line[:start] + value + line[end:], nil
}

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// polecatPolicy reads deploy/polecat/policy.yaml, the sandbox policy the
// openshell backend sends with every sandbox, relative to this source file.
func polecatPolicy(t *testing.T) map[string]any {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "polecat", "policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("policy.yaml does not parse: %v", err)
	}
	return doc
}

// policyEndpoint is one network_policies entry: the group that declares it, the
// binaries allowed to reach it, and the endpoint's own fields.
type policyEndpoint struct {
	group string
	bins  []string
	doc   map[string]any
}

// policyEndpoints maps each network policy's host:port to the endpoint that
// allows it, failing the test when the policy is malformed.
func policyEndpoints(t *testing.T, doc map[string]any) map[string]policyEndpoint {
	t.Helper()
	out, err := parsePolicyEndpoints(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// parsePolicyEndpoints walks network_policies. Every field is read with a
// checked assertion so a malformed policy names the offending field instead of
// panicking mid-test (gt-xe8we). A host:port declared twice is an error naming
// both groups: the same host under two groups hides which group's binaries and
// rules win.
func parsePolicyEndpoints(doc map[string]any) (map[string]policyEndpoint, error) {
	groups, ok := doc["network_policies"].(map[string]any)
	if !ok || len(groups) == 0 {
		return nil, fmt.Errorf("policy network_policies: want a non-empty mapping, got %T", doc["network_policies"])
	}
	out := make(map[string]policyEndpoint, len(groups))
	owner := make(map[string]string, len(groups))
	for name, g := range groups {
		group, ok := g.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("network_policies.%s: want a mapping, got %T", name, g)
		}
		bins, err := parsePolicyBinaries(name, group["binaries"])
		if err != nil {
			return nil, err
		}
		endpoints, ok := group["endpoints"].([]any)
		if !ok {
			return nil, fmt.Errorf("network_policies.%s.endpoints: want a list, got %T", name, group["endpoints"])
		}
		for i, e := range endpoints {
			ep, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("network_policies.%s.endpoints[%d]: want a mapping, got %T", name, i, e)
			}
			key := fmt.Sprintf("%v:%v", ep["host"], ep["port"])
			if first, dup := owner[key]; dup {
				if first == name {
					return nil, fmt.Errorf("endpoint %s appears twice in group %s", key, name)
				}
				return nil, fmt.Errorf("endpoint %s is allowed by both %s and %s", key, first, name)
			}
			owner[key] = name
			out[key] = policyEndpoint{group: name, bins: bins, doc: ep}
		}
	}
	return out, nil
}

// parsePolicyBinaries collects the path of each binary a group allows.
func parsePolicyBinaries(group string, v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("network_policies.%s.binaries: want a list, got %T", group, v)
	}
	bins := make([]string, 0, len(list))
	for i, b := range list {
		bin, ok := b.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("network_policies.%s.binaries[%d]: want a mapping, got %T", group, i, b)
		}
		path, ok := bin["path"].(string)
		if !ok {
			return nil, fmt.Errorf("network_policies.%s.binaries[%d].path: want a string, got %T", group, i, bin["path"])
		}
		bins = append(bins, path)
	}
	return bins, nil
}

// reviewedEndpoint is the reviewed shape of one allowed host:port. An empty
// access leaves the policy's default in force, and dolt alone has no
// enforcement field because its MySQL handshake offers no request to inspect.
type reviewedEndpoint struct {
	protocol    string
	access      string
	enforcement string
}

// The sandbox's egress is deny-by-default and exactly this list: the control
// plane on the Mac, the Go module hosts, the npm registry and the two Google
// font hosts next/font/google fetches at build time. Each endpoint is pinned by
// protocol, access and enforcement too, so narrowing one of those fields is as
// reviewable as adding a host (gt-xe8we).
var reviewedEgress = map[string]reviewedEndpoint{
	"host.docker.internal:3000":  {protocol: "rest", enforcement: "enforce"},
	"host.docker.internal:3307":  {protocol: "tcp"},
	"proxy.golang.org:443":       {protocol: "rest", access: "read-only", enforcement: "enforce"},
	"sum.golang.org:443":         {protocol: "rest", access: "read-only", enforcement: "enforce"},
	"storage.googleapis.com:443": {protocol: "rest", access: "read-only", enforcement: "enforce"},
	"registry.npmjs.org:443":     {protocol: "rest", access: "read-only", enforcement: "enforce"},
	"fonts.googleapis.com:443":   {protocol: "rest", access: "read-only", enforcement: "enforce"},
	"fonts.gstatic.com:443":      {protocol: "rest", access: "read-only", enforcement: "enforce"},
}

func TestPolecatPolicyAllowsOnlyTheReviewedEgress(t *testing.T) {
	t.Parallel()
	doc := polecatPolicy(t)
	if v, _ := doc["version"].(int); v != 1 {
		t.Errorf("policy version = %v, want 1", doc["version"])
	}
	got := policyEndpoints(t, doc)
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := make([]string, 0, len(reviewedEgress))
	for k := range reviewedEgress {
		want = append(want, k)
	}
	sort.Strings(want)
	if len(keys) != len(want) {
		t.Fatalf("policy endpoints = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("policy endpoints = %v, want %v", keys, want)
		}
	}
	for _, key := range want {
		ep := got[key]
		reviewed := reviewedEgress[key]
		for _, field := range []struct{ name, want string }{
			{"protocol", reviewed.protocol},
			{"access", reviewed.access},
			{"enforcement", reviewed.enforcement},
		} {
			if got := policyField(ep.doc[field.name]); got != field.want {
				t.Errorf("%s (%s) %s = %q, want %q", key, ep.group, field.name, got, field.want)
			}
		}
	}
}

// policyField renders a policy scalar for comparison; an absent field is "".
func policyField(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// A malformed policy must name the offending field, not panic on an unchecked
// assertion (gt-xe8we).
func TestParsePolicyEndpointsReportsMalformedPolicy(t *testing.T) {
	t.Parallel()
	binaries := []any{map[string]any{"path": "/usr/bin/node"}}
	endpoints := []any{map[string]any{"host": "registry.npmjs.org", "port": 443}}
	group := func(bins, eps any) map[string]any {
		return map[string]any{"network_policies": map[string]any{
			"npm": map[string]any{"binaries": bins, "endpoints": eps},
		}}
	}
	cases := []struct {
		name string
		doc  map[string]any
		want string // field the error must name
	}{
		{"no network_policies", map[string]any{}, "network_policies"},
		{"group is not a mapping", map[string]any{"network_policies": map[string]any{"npm": "nope"}}, "network_policies.npm"},
		{"binaries is not a list", group("nope", endpoints), "network_policies.npm.binaries"},
		{"binary is not a mapping", group([]any{"nope"}, endpoints), "network_policies.npm.binaries[0]"},
		{"binary path is not a string", group([]any{map[string]any{"path": 1}}, endpoints), "network_policies.npm.binaries[0].path"},
		{"endpoints is not a list", group(binaries, "nope"), "network_policies.npm.endpoints"},
		{"endpoint is not a mapping", group(binaries, []any{"nope"}), "network_policies.npm.endpoints[0]"},
		{"endpoint declared twice", map[string]any{"network_policies": map[string]any{
			"npm":  map[string]any{"binaries": binaries, "endpoints": endpoints},
			"npm2": map[string]any{"binaries": binaries, "endpoints": endpoints},
		}}, "registry.npmjs.org:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parsePolicyEndpoints(tc.doc)
			if err == nil {
				t.Fatalf("parsePolicyEndpoints accepted a policy whose %s is malformed", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// npm and next build run as node scripts, so node is the binary allowed to reach
// the registry and the font hosts; no other binary may.
func TestPolecatPolicyLetsOnlyNodeReachTheNpmRegistryAndFonts(t *testing.T) {
	t.Parallel()
	got := policyEndpoints(t, polecatPolicy(t))
	for _, key := range []string{"registry.npmjs.org:443", "fonts.googleapis.com:443", "fonts.gstatic.com:443"} {
		bins := got[key].bins
		if len(bins) != 1 || bins[0] != "/usr/bin/node" {
			t.Errorf("%s binaries = %v, want only /usr/bin/node", key, bins)
		}
	}
}

// policyPaths returns the paths the policy grants under filesystem_policy.<key>.
func policyPaths(t *testing.T, doc map[string]any, key string) []string {
	t.Helper()
	fs, ok := doc["filesystem_policy"].(map[string]any)
	if !ok {
		t.Fatal("policy has no filesystem_policy")
	}
	list, _ := fs[key].([]any)
	var out []string
	for _, p := range list {
		out = append(out, fmt.Sprint(p))
	}
	return out
}

// DuckDB reads the cgroup CPU limit to pick its default thread count and aborts
// the process when that read is denied, so the cgroup tree is readable and
// never writable (gt-vhkxd).
func TestPolecatPolicyLetsTheCgroupTreeBeReadNotWritten(t *testing.T) {
	t.Parallel()
	doc := polecatPolicy(t)
	const cgroup = "/sys/fs/cgroup"
	contains := func(list []string) bool {
		for _, p := range list {
			if p == cgroup {
				return true
			}
		}
		return false
	}
	if !contains(policyPaths(t, doc, "read_only")) {
		t.Errorf("filesystem_policy.read_only = %v, want it to list %s", policyPaths(t, doc, "read_only"), cgroup)
	}
	if contains(policyPaths(t, doc, "read_write")) {
		t.Errorf("filesystem_policy.read_write lists %s: the cgroup tree must stay read only", cgroup)
	}
}

// /dev/zero is readable and never writable: `head -c N /dev/zero` and
// `dd if=/dev/zero` failed with permission denied in a sandbox (gt-8yigq).
func TestPolecatPolicyLetsDevZeroBeReadNotWritten(t *testing.T) {
	t.Parallel()
	doc := polecatPolicy(t)
	const zero = "/dev/zero"
	has := func(list []string) bool {
		for _, p := range list {
			if p == zero {
				return true
			}
		}
		return false
	}
	if !has(policyPaths(t, doc, "read_only")) {
		t.Errorf("filesystem_policy.read_only = %v, want it to list %s", policyPaths(t, doc, "read_only"), zero)
	}
	if has(policyPaths(t, doc, "read_write")) {
		t.Errorf("filesystem_policy.read_write lists %s: it must stay read only", zero)
	}
}

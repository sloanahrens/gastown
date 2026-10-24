package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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

// policyEndpoints maps each network policy's host:port to the binaries allowed to
// reach it.
func policyEndpoints(t *testing.T, doc map[string]any) map[string][]string {
	t.Helper()
	groups, _ := doc["network_policies"].(map[string]any)
	if len(groups) == 0 {
		t.Fatal("policy.yaml has no network_policies")
	}
	out := map[string][]string{}
	for name, g := range groups {
		group, _ := g.(map[string]any)
		var bins []string
		for _, b := range group["binaries"].([]any) {
			bins = append(bins, b.(map[string]any)["path"].(string))
		}
		for _, e := range group["endpoints"].([]any) {
			ep := e.(map[string]any)
			key := fmt.Sprintf("%v:%v", ep["host"], ep["port"])
			if _, dup := out[key]; dup {
				t.Errorf("endpoint %s is allowed by more than one group (%s)", key, name)
			}
			out[key] = bins
		}
	}
	return out
}

// The sandbox's egress is deny-by-default and exactly this list: the control
// plane on the Mac, the Go module hosts, the npm registry and the two Google
// font hosts next/font/google fetches at build time. A host added to the policy
// without being added here is a widening nobody reviewed.
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
	want := []string{
		"fonts.googleapis.com:443", "fonts.gstatic.com:443",
		"host.docker.internal:3000", "host.docker.internal:3307",
		"proxy.golang.org:443", "registry.npmjs.org:443",
		"storage.googleapis.com:443", "sum.golang.org:443",
	}
	if len(keys) != len(want) {
		t.Fatalf("policy endpoints = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("policy endpoints = %v, want %v", keys, want)
			break
		}
	}
}

// npm and next build run as node scripts, so node is the binary allowed to reach
// the registry and the font hosts; no other binary may.
func TestPolecatPolicyLetsOnlyNodeReachTheNpmRegistryAndFonts(t *testing.T) {
	t.Parallel()
	got := policyEndpoints(t, polecatPolicy(t))
	for _, key := range []string{"registry.npmjs.org:443", "fonts.googleapis.com:443", "fonts.gstatic.com:443"} {
		bins := got[key]
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

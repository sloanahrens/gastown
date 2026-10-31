package gtcache

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// claimFile is the manifest this package guards.
const claimFile = "claim.yaml"

// readClaim parses claimFile as one object. yaml.Unmarshal refuses a stream of
// more than one document, so a second object in the file fails the test.
func readClaim(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(claimFile)
	if err != nil {
		t.Fatalf("reading %s: %v", claimFile, err)
	}
	var claim map[string]any
	if err := yaml.Unmarshal(raw, &claim); err != nil {
		t.Fatalf("%s is not one YAML object: %v", claimFile, err)
	}
	return claim
}

// at walks nested maps by key and fails the test when a step is missing or is
// not a map.
func at(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, k := range path {
		next, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v: %q is not a map on the way to %v", cur, k, path)
		}
		if cur, ok = next[k]; !ok {
			t.Fatalf("missing key %q on the path %v", k, path)
		}
	}
	return cur
}

// TestClaimManifest pins the claim every sandbox pod mounts: its kind, its name
// and namespace, the access mode, the operator-approved 20Gi cap, and the
// storageClassName left absent so the cluster default decides where it binds
// (gt-27sh1).
func TestClaimManifest(t *testing.T) {
	t.Parallel()
	claim := readClaim(t)
	for _, c := range []struct {
		name string
		got  any
		want any
	}{
		{"apiVersion", claim["apiVersion"], "v1"},
		{"kind", claim["kind"], "PersistentVolumeClaim"},
		{"metadata.name", at(t, claim, "metadata", "name"), "gt-cache"},
		{"metadata.namespace", at(t, claim, "metadata", "namespace"), "openshell"},
		{"spec.accessModes", at(t, claim, "spec", "accessModes"), []any{"ReadWriteOnce"}},
		{"spec.resources.requests.storage", at(t, claim, "spec", "resources", "requests", "storage"), "20Gi"},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	spec := at(t, claim, "spec")
	if v, ok := spec.(map[string]any)["storageClassName"]; ok {
		t.Errorf("spec.storageClassName = %v, want none: the claim takes the cluster default", v)
	}
	wantLabels := map[string]any{
		"app.kubernetes.io/managed-by": "gt",
		"app.kubernetes.io/name":       "gt-cache",
	}
	if got := at(t, claim, "metadata", "labels"); !reflect.DeepEqual(got, wantLabels) {
		t.Errorf("metadata.labels = %v, want %v", got, wantLabels)
	}
}

// TestClaimHeaderStatesTheBindingRule reads the header an operator applies the
// file by: the default class binds on the first consumer, so every consumer
// sits on the sandbox pool (gt-27sh1).
func TestClaimHeaderStatesTheBindingRule(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(claimFile)
	if err != nil {
		t.Fatalf("reading %s: %v", claimFile, err)
	}
	header := commentHeader(raw)
	for _, want := range []struct{ claim, text string }{
		{"the first-consumer binding rule", "WaitForFirstConsumer"},
		{"the sandbox pool", "gt.dev/pool=sandbox"},
	} {
		if !strings.Contains(header, want.text) {
			t.Errorf("the header does not name %s: it must contain %q, and the header is:\n%s", want.claim, want.text, header)
		}
	}
}

// commentHeader returns the comment block at the top of raw: the lines before
// the first that is not a comment.
func commentHeader(raw []byte) string {
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

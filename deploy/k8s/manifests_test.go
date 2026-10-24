package k8s

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// decodeDocs parses every document of a multi-document YAML file. A file that
// is not valid YAML fails here, which a grep over the text cannot tell.
func decodeDocs(t *testing.T, rel string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("%s is not valid YAML: %v", rel, err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
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

func TestPriorityClassesOrderControlPlaneAboveSandboxAboveCI(t *testing.T) {
	t.Parallel()
	docs := decodeDocs(t, "priority-classes.yaml")
	got := map[string]int{}
	for _, d := range docs {
		if d["kind"] != "PriorityClass" {
			t.Errorf("document kind = %v, want PriorityClass", d["kind"])
		}
		if g, _ := d["globalDefault"].(bool); g {
			t.Errorf("%v is a global default; no class may be", at(t, d, "metadata", "name"))
		}
		if desc, _ := d["description"].(string); desc == "" {
			t.Errorf("%v has no description", at(t, d, "metadata", "name"))
		}
		got[at(t, d, "metadata", "name").(string)] = d["value"].(int)
	}
	want := map[string]int{"gt-control-plane": 1000000, "gt-sandbox": 1000, "gt-ci": 100}
	if len(got) != len(want) {
		t.Fatalf("classes = %v, want exactly %v", got, want)
	}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("%s value = %d, want %d", name, got[name], v)
		}
	}
	if !(got["gt-control-plane"] > got["gt-sandbox"] && got["gt-sandbox"] > got["gt-ci"]) {
		t.Errorf("eviction order broken: %v", got)
	}
}

func TestKindValuesPinGatewayToCoreAndAllowDriverConfig(t *testing.T) {
	t.Parallel()
	docs := decodeDocs(t, "values/kind.yaml")
	if len(docs) != 1 {
		t.Fatalf("values/kind.yaml has %d documents, want 1", len(docs))
	}
	v := docs[0]
	if got := at(t, v, "nodeSelector", "gt.dev/pool"); got != "core" {
		t.Errorf("gateway nodeSelector gt.dev/pool = %v, want core", got)
	}
	if got := at(t, v, "server", "drivers", "kubernetes", "allowDriverConfig"); got != true {
		t.Errorf("server.drivers.kubernetes.allowDriverConfig = %v, want true", got)
	}
	if got := at(t, v, "server", "auth", "allowUnauthenticatedUsers"); got != true {
		t.Errorf("server.auth.allowUnauthenticatedUsers = %v, want true (kind is mTLS on loopback)", got)
	}
}

// The mutable gastown-polecat:dev tag is only re-pulled under Always (gt-t39cu).
// global.image.pullPolicy would not do: the chart applies it to the runtime
// and supervisor images, never the sandbox workload image.
func TestKindValuesPullSandboxImageAlways(t *testing.T) {
	t.Parallel()
	v := decodeDocs(t, "values/kind.yaml")[0]
	if got := at(t, v, "sandbox", "image", "pullPolicy"); got != "Always" {
		t.Errorf("sandbox.image.pullPolicy = %v, want Always", got)
	}
}

func TestStaticManifestsParse(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{"kind/gateway-nodeport.yaml", "services/control-plane.yaml", "policy/kind.yaml"} {
		if len(decodeDocs(t, rel)) == 0 {
			t.Errorf("%s has no documents", rel)
		}
	}
}

func TestControlPlaneServicesAreExternalNames(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, d := range decodeDocs(t, "services/control-plane.yaml") {
		name := at(t, d, "metadata", "name").(string)
		seen[name] = true
		if got := at(t, d, "spec", "type"); got != "ExternalName" {
			t.Errorf("service %s type = %v, want ExternalName", name, got)
		}
		if got := at(t, d, "spec", "externalName"); got != "host.lima.internal" {
			t.Errorf("service %s externalName = %v, want host.lima.internal", name, got)
		}
	}
	for _, want := range []string{"control-plane", "dolt", "forgejo"} {
		if !seen[want] {
			t.Errorf("service %s is missing", want)
		}
	}
}

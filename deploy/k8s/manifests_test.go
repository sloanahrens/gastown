package k8s

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// The landing gate outranks a tier sweep, which is what puts a gate ahead of a
// sweep in the queue when the cluster is short of room (gt-xaokd).
func TestPriorityClassesDefineTheGateClass(t *testing.T) {
	t.Parallel()
	var found bool
	for _, d := range decodeDocs(t, "priority-classes.yaml") {
		if at(t, d, "metadata", "name") != "gt-ci-gate" {
			continue
		}
		found = true
		if got := d["value"]; got != 200 {
			t.Errorf("gt-ci-gate value = %v, want 200", got)
		}
		if got, ok := d["globalDefault"]; !ok || got != false {
			t.Errorf("gt-ci-gate globalDefault = %v, want false", got)
		}
	}
	if !found {
		t.Fatal("priority-classes.yaml has no gt-ci-gate PriorityClass")
	}
}

func TestPriorityClassOrderSweepGateSandboxControlPlane(t *testing.T) {
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
		nameAny := at(t, d, "metadata", "name")
		name, ok := nameAny.(string)
		if !ok {
			t.Fatalf("metadata.name = %v (%T), want a string", nameAny, nameAny)
		}
		value, ok := d["value"].(int)
		if !ok {
			t.Fatalf("%s value = %v (%T), want an int", name, d["value"], d["value"])
		}
		got[name] = value
	}
	want := map[string]int{"gt-control-plane": 1000000, "gt-sandbox": 1000, "gt-ci-gate": 200, "gt-ci": 100}
	if len(got) != len(want) {
		t.Fatalf("classes = %v, want exactly %v", got, want)
	}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("%s value = %d, want %d", name, got[name], v)
		}
	}
	if !(got["gt-control-plane"] > got["gt-sandbox"] && got["gt-sandbox"] > got["gt-ci-gate"] && got["gt-ci-gate"] > got["gt-ci"]) {
		t.Errorf("eviction order broken: %v", got)
	}
}

// The gateway admits a PriorityClass a sandbox create names only with its
// approval label (resourceAdmission); gt-sandbox is the one sandboxes name.
func TestSandboxPriorityClassCarriesAttachableLabel(t *testing.T) {
	t.Parallel()
	var found bool
	for _, d := range decodeDocs(t, "priority-classes.yaml") {
		nameAny := at(t, d, "metadata", "name")
		name, ok := nameAny.(string)
		if !ok {
			t.Fatalf("metadata.name = %v (%T), want a string", nameAny, nameAny)
		}
		if name != "gt-sandbox" {
			continue
		}
		found = true
		metaAny := at(t, d, "metadata")
		meta, ok := metaAny.(map[string]any)
		if !ok {
			t.Fatalf("gt-sandbox metadata = %v (%T), want a map", metaAny, metaAny)
		}
		labels, ok := meta["labels"].(map[string]any)
		if !ok {
			t.Fatalf("gt-sandbox metadata.labels = %v (%T), want a map", meta["labels"], meta["labels"])
		}
		if got := labels["openshell.ai/sandbox-attachable"]; got != "true" {
			t.Errorf("gt-sandbox label openshell.ai/sandbox-attachable = %v, want \"true\"", got)
		}
	}
	if !found {
		t.Fatal("priority-classes.yaml has no gt-sandbox PriorityClass")
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
	for _, rel := range []string{"kind/gateway-nodeport.yaml", "values/kind/control-plane-services.yaml", "values/kind/hosts.yaml", "values/cloud/control-plane-services.yaml", "values/cloud/hosts.yaml", "policy/kind.yaml"} {
		if len(decodeDocs(t, rel)) == 0 {
			t.Errorf("%s has no documents", rel)
		}
	}
}

func TestControlPlaneServicesAreExternalNames(t *testing.T) {
	t.Parallel()
	for _, env := range []string{"kind", "cloud"} {
		want := decodeDocs(t, "values/"+env+"/hosts.yaml")[0]["control_plane_host"]
		seen := map[string]bool{}
		for _, d := range decodeDocs(t, "values/"+env+"/control-plane-services.yaml") {
			name := at(t, d, "metadata", "name").(string)
			seen[name] = true
			if got := at(t, d, "spec", "type"); got != "ExternalName" {
				t.Errorf("%s: service %s type = %v, want ExternalName", env, name, got)
			}
			if got := at(t, d, "spec", "externalName"); got != want {
				t.Errorf("%s: service %s externalName = %v, want the hosts file's control_plane_host %v", env, name, got, want)
			}
		}
		for _, w := range []string{"control-plane", "dolt", "forgejo", "tracker"} {
			if !seen[w] {
				t.Errorf("%s: service %s is missing", env, w)
			}
		}
	}
}

// A move to the cloud is a swap of one values directory, so the two
// environments must name the same keys, each with a value, and a stateful claim
// must name a reclaim policy the cluster accepts.
func TestHostsValuesFilesCarrySameKeys(t *testing.T) {
	t.Parallel()
	kind := decodeDocs(t, "values/kind/hosts.yaml")[0]
	cloud := decodeDocs(t, "values/cloud/hosts.yaml")[0]
	for k := range kind {
		if _, ok := cloud[k]; !ok {
			t.Errorf("values/cloud/hosts.yaml lacks key %q that values/kind/hosts.yaml has", k)
		}
	}
	for k := range cloud {
		if _, ok := kind[k]; !ok {
			t.Errorf("values/kind/hosts.yaml lacks key %q that values/cloud/hosts.yaml has", k)
		}
	}
	for _, key := range []string{"control_plane_host", "registry_host", "storage_class", "reclaim_policy"} {
		for env, doc := range map[string]map[string]any{"kind": kind, "cloud": cloud} {
			if v, _ := doc[key].(string); v == "" {
				t.Errorf("values/%s/hosts.yaml: %s is missing or empty", env, key)
			}
		}
	}
	for env, doc := range map[string]map[string]any{"kind": kind, "cloud": cloud} {
		if r := doc["reclaim_policy"]; r != "Delete" && r != "Retain" {
			t.Errorf("values/%s/hosts.yaml: reclaim_policy = %v, want Delete or Retain", env, r)
		}
	}
}

// hardCodedAddresses returns the files below root that spell an environment's
// address instead of reading it from a values file. The values directory and
// testdata hold environments by design, and a *_test.go file may name the
// strings it guards.
func hardCodedAddresses(t *testing.T, root string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel, _ := filepath.Rel(root, path); rel == "values" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, bad := range []string{"localhost:5001", "host.lima.internal"} {
			if bytes.Contains(raw, []byte(bad)) {
				rel, _ := filepath.Rel(root, path)
				hits = append(hits, rel+" names "+bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return hits
}

func TestNoHardCodedRegistryOrHostLimaInManifests(t *testing.T) {
	t.Parallel()
	for _, h := range hardCodedAddresses(t, ".") {
		t.Errorf("%s: move the address into values/<env>/hosts.yaml and read it from there", h)
	}
}

// The guard has to go red when an address is spelled in a manifest, and stay
// quiet for the values directory.
func TestHardCodedAddressGuardFiresOnAPlantedAddress(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for rel, text := range map[string]string{
		"a/ok.yaml":          "image: from-values\n",
		"a/bad.yaml":         "image: localhost:5001/x\n",
		"values/kind/h.yaml": "host: host.lima.internal\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := hardCodedAddresses(t, root)
	if len(got) != 1 || !strings.Contains(got[0], "bad.yaml") {
		t.Fatalf("guard hits = %v, want exactly a/bad.yaml", got)
	}
}

// The Docker policy is the source the kind policy renders from, so its tracker
// endpoint must reach trackerd on the default port from both gt and bd.
func TestDockerPolicyTrackerEndpointReachesGtAndBd(t *testing.T) {
	t.Parallel()
	pol := decodeDocs(t, "../polecat/policy.yaml")[0]
	eps, ok := at(t, pol, "network_policies", "tracker", "endpoints").([]any)
	if !ok || len(eps) != 1 {
		t.Fatalf("tracker endpoints = %v, want exactly one endpoint", eps)
	}
	ep, ok := eps[0].(map[string]any)
	if !ok {
		t.Fatalf("tracker endpoint = %v, want a map", eps[0])
	}
	if got := ep["host"]; got != "host.docker.internal" {
		t.Errorf("tracker host = %v, want host.docker.internal", got)
	}
	if got := ep["port"]; got != 7433 {
		t.Errorf("tracker port = %v, want 7433", got)
	}
	bins, ok := at(t, pol, "network_policies", "tracker", "binaries").([]any)
	if !ok {
		t.Fatalf("tracker binaries = %v, want a list", bins)
	}
	paths := map[string]bool{}
	for _, b := range bins {
		if m, ok := b.(map[string]any); ok {
			if p, ok := m["path"].(string); ok {
				paths[p] = true
			}
		}
	}
	for _, want := range []string{"/usr/local/bin/gt", "/usr/local/bin/bd"} {
		if !paths[want] {
			t.Errorf("tracker binaries %v lack %s", paths, want)
		}
	}
}

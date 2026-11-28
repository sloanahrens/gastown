package forgejoserver

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// manifestDocs lists the documents server.yaml holds, in file order, each with
// the recorded object it must rebuild.
var manifestDocs = []struct{ file, kind, name string }{
	{"namespace.json", "Namespace", "forgejo"},
	{"claim.json", "PersistentVolumeClaim", "data"},
	{"service.json", "Service", "forgejo"},
	{"deployment.json", "Deployment", "forgejo"},
}

// recordedCPU and manifestCPU are the requests.cpu the record holds and the
// manifest writes instead (gt-1x40gk).
const recordedCPU, manifestCPU = "500m", "750m"

// kindHost returns the kind environment's control_plane_host, the value the
// manifest is rendered with.
func kindHost(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../values/kind/hosts.yaml")
	if err != nil {
		t.Fatalf("reading the kind hosts file: %v", err)
	}
	var hosts map[string]any
	if err := yaml.Unmarshal(raw, &hosts); err != nil {
		t.Fatalf("decoding the kind hosts file: %v", err)
	}
	host, _ := hosts["control_plane_host"].(string)
	if host == "" {
		t.Fatal("the kind hosts file names no control_plane_host")
	}
	return host
}

// jsonDecoded returns v after a JSON encode and decode, so a number is the
// float64 encoding/json reads on both sides of a comparison.
func jsonDecoded(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding %v: %v", v, err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return out
}

// field walks nested maps by key and fails the test when a step is missing.
func field(t *testing.T, obj map[string]any, path ...string) any {
	t.Helper()
	var cur any = obj
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v is not a map on the way to %v", cur, path)
		}
		if cur, ok = m[k]; !ok {
			t.Fatalf("missing key %q on the path %v", k, path)
		}
	}
	return cur
}

// forgejoContainer returns the one container of a Deployment's pod template.
func forgejoContainer(t *testing.T, deployment map[string]any) map[string]any {
	t.Helper()
	containers, ok := field(t, deployment, "spec", "template", "spec", "containers").([]any)
	if !ok || len(containers) != 1 {
		t.Fatalf("containers = %v, want one container", containers)
	}
	c, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("container = %v, want a map", containers[0])
	}
	return c
}

// forgejoRequests returns the requests map of a Deployment's one container.
func forgejoRequests(t *testing.T, deployment map[string]any) map[string]any {
	t.Helper()
	c := forgejoContainer(t, deployment)
	requests, ok := field(t, c, "resources", "requests").(map[string]any)
	if !ok {
		t.Fatalf("requests = %v, want a map", field(t, c, "resources", "requests"))
	}
	return requests
}

// databaseHostEnv returns the FORGEJO__database__HOST entry of a Deployment's one container.
func databaseHostEnv(t *testing.T, deployment map[string]any) map[string]any {
	t.Helper()
	c := forgejoContainer(t, deployment)
	envs, ok := field(t, c, "env").([]any)
	if !ok {
		t.Fatalf("env = %v, want a list", field(t, c, "env"))
	}
	for _, e := range envs {
		if m, ok := e.(map[string]any); ok && m["name"] == "FORGEJO__database__HOST" {
			return m
		}
	}
	t.Fatal("the container has no FORGEJO__database__HOST entry")
	return nil
}

// objects returns the manifest's documents rendered for controlPlaneHost,
// failing unless there is one per manifestDocs row.
func objects(t *testing.T, controlPlaneHost string) []map[string]any {
	t.Helper()
	objs, err := Objects(controlPlaneHost)
	if err != nil {
		t.Fatalf("Objects(%q): %v", controlPlaneHost, err)
	}
	if len(objs) != len(manifestDocs) {
		t.Fatalf("Objects() returned %d documents, want %d", len(objs), len(manifestDocs))
	}
	return objs
}

func TestManifestMatchesRecorded(t *testing.T) {
	t.Parallel()
	host := kindHost(t)
	objs := objects(t, host)
	for i, want := range manifestDocs {
		t.Run(want.kind+" "+want.name, func(t *testing.T) {
			got := objs[i]
			if got["kind"] != want.kind || field(t, got, "metadata", "name") != want.name {
				t.Fatalf("document %d is %v %v, want %s %s", i+1, got["kind"], field(t, got, "metadata", "name"), want.kind, want.name)
			}
			recorded := Strip(loadLive(t, want.file))
			if want.kind == "Deployment" {
				requests := forgejoRequests(t, recorded)
				if requests["cpu"] != recordedCPU {
					t.Fatalf("recorded requests.cpu = %v, want %s", requests["cpu"], recordedCPU)
				}
				requests["cpu"] = manifestCPU
				dbHost := databaseHostEnv(t, recorded)
				if v, _ := dbHost["value"].(string); v != host+":5442" {
					t.Fatalf("recorded FORGEJO__database__HOST = %v, want the hosts file's %s:5442", dbHost["value"], host)
				}
			}
			if w, g := jsonDecoded(t, recorded), jsonDecoded(t, got); !reflect.DeepEqual(w, g) {
				t.Errorf("%s %s differs from its recorded object\nrecord:   %v\nmanifest: %v", want.kind, want.name, w, g)
			}
		})
	}
}

// TestManifestRefuses drives every refusal branch: a host no environment may
// name, and a file whose placeholder is absent or holds twice.
func TestManifestRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, host string }{
		{"empty", ""},
		{"over-long", strings.Repeat("a", 254)},
		{"malformed", "-bad"},
		{"unfilled placeholder", "CONTROL_PLANE_HOST"},
	} {
		t.Run("host "+tc.name, func(t *testing.T) {
			t.Parallel()
			if raw, err := Manifest(tc.host); err == nil {
				t.Fatalf("Manifest(%q) = %q, want an error", tc.host, raw)
			}
		})
	}
	for _, tc := range []struct{ name, raw string }{
		{"absent", "no database host here\n"},
		{"twice", databaseHostPlaceholder + "\n" + databaseHostPlaceholder + "\n"},
	} {
		t.Run("placeholder "+tc.name, func(t *testing.T) {
			t.Parallel()
			if raw, err := fillHost([]byte(tc.raw), "host.example"); err == nil {
				t.Fatalf("fillHost(%q) = %q, want an error", tc.raw, raw)
			}
		})
	}
}

// TestObjectsRefusesAHost checks Objects propagates Manifest's refusal rather
// than decoding the unfilled placeholder.
func TestObjectsRefusesAHost(t *testing.T) {
	t.Parallel()
	if objs, err := Objects("CONTROL_PLANE_HOST"); err == nil {
		t.Fatalf("Objects(CONTROL_PLANE_HOST) = %v, want an error", objs)
	}
}

func TestDecodeObjectsRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, raw string }{
		{"not YAML", "a: [\n"},
		{"empty document", "~\n"},
		{"scalar document", "just text\n"},
		{"list document", "- a\n- b\n"},
		{"value JSON cannot encode", "a: .nan\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if objs, err := decodeObjects([]byte(tc.raw)); err == nil {
				t.Fatalf("decodeObjects accepted %q as %v, want an error", tc.raw, objs)
			}
		})
	}
}

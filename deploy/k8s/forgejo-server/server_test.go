package forgejoserver

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
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
// manifest writes instead (gt-1x40gk). manifestDBHost replaces the record's
// database host, a host followed by :5442, with a placeholder: no manifest
// spells a real host (gt-3zlo1f fills it in).
const recordedCPU, manifestCPU = "500m", "750m"
const manifestDBHost = "CONTROL_PLANE_HOST:5442"

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

// objects returns server.yaml's documents, failing unless there is one per manifestDocs row.
func objects(t *testing.T) []map[string]any {
	t.Helper()
	objs, err := Objects()
	if err != nil {
		t.Fatalf("Objects(): %v", err)
	}
	if len(objs) != len(manifestDocs) {
		t.Fatalf("Objects() returned %d documents, want %d", len(objs), len(manifestDocs))
	}
	return objs
}

func TestManifestMatchesRecorded(t *testing.T) {
	t.Parallel()
	objs := objects(t)
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
				host := databaseHostEnv(t, recorded)
				if v, ok := host["value"].(string); !ok || v == ":5442" || !strings.HasSuffix(v, ":5442") {
					t.Fatalf("recorded FORGEJO__database__HOST = %v, want a host followed by :5442", host["value"])
				}
				host["value"] = manifestDBHost
			}
			if w, g := jsonDecoded(t, recorded), jsonDecoded(t, got); !reflect.DeepEqual(w, g) {
				t.Errorf("%s %s differs from its recorded object\nrecord:   %v\nmanifest: %v", want.kind, want.name, w, g)
			}
		})
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

package forgejoserver

import (
	"encoding/json"
	"reflect"
	"testing"
)

// manifestDocs lists the documents server.yaml holds, in file order, each with
// the recorded object it must rebuild.
var manifestDocs = []struct{ file, kind, name string }{
	{"namespace.json", "Namespace", "forgejo"},
	{"claim.json", "PersistentVolumeClaim", "data"},
	{"service.json", "Service", "forgejo"},
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if objs, err := decodeObjects([]byte(tc.raw)); err == nil {
				t.Fatalf("decodeObjects accepted %q as %v, want an error", tc.raw, objs)
			}
		})
	}
}

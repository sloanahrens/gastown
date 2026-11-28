package forgejoserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func loadLive(t *testing.T, file string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "live-2026-11-27", file))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// has reports whether the dotted path exists in obj; a numeric segment indexes
// a slice.
func has(obj map[string]any, path string) bool {
	var cur any = obj
	for _, k := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[k]
			if !ok {
				return false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(v) {
				return false
			}
			cur = v[i]
		default:
			return false
		}
	}
	return true
}

func TestStrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file    string
		absent  []string
		present []string
	}{
		{"deployment.json",
			[]string{"status", "metadata.uid", "metadata.resourceVersion", "metadata.generation",
				"metadata.creationTimestamp", "metadata.managedFields", "metadata.finalizers",
				"metadata.annotations"},
			[]string{"metadata.name", "spec.selector", "spec.strategy",
				"spec.template.spec.priorityClassName", "spec.template.spec.tolerations",
				"spec.template.spec.securityContext", "spec.template.spec.containers.0.env",
				"spec.template.spec.containers.0.readinessProbe",
				"spec.template.spec.containers.0.resources",
				"spec.template.spec.containers.0.ports"}},
		{"service.json",
			[]string{"status", "metadata.uid", "metadata.annotations", "spec.clusterIP",
				"spec.clusterIPs", "spec.ports.0.nodePort", "spec.ports.1.nodePort"},
			[]string{"metadata.name", "spec.selector", "spec.ports.0.port", "spec.ports.1.port"}},
		{"claim.json",
			[]string{"status", "metadata.uid", "metadata.annotations", "metadata.finalizers",
				"spec.volumeName"},
			[]string{"metadata.name", "spec.storageClassName", "spec.resources.requests.storage"}},
		{"namespace.json",
			[]string{"status", "metadata.uid", "metadata.labels", "spec"},
			[]string{"metadata.name"}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			obj := loadLive(t, tc.file)
			got := Strip(obj)
			for _, p := range tc.absent {
				if has(got, p) {
					t.Errorf("%s survives the strip", p)
				}
			}
			for _, p := range tc.present {
				if !has(got, p) {
					t.Errorf("%s is gone after the strip", p)
				}
			}
			if !reflect.DeepEqual(obj, loadLive(t, tc.file)) {
				t.Error("Strip changed its input")
			}
		})
	}

	t.Run("template metadata", func(t *testing.T) {
		got := Strip(map[string]any{"kind": "Deployment", "spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{"creationTimestamp": "x",
				"annotations": map[string]any{"kubectl.kubernetes.io/restartedAt": "x"}},
			"spec": map[string]any{"containers": []any{}},
		}}})
		if has(got, "spec.template.metadata") || !has(got, "spec.template.spec") {
			t.Errorf("template after the strip: %v", got)
		}
	})
}

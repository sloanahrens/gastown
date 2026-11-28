package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func forgejoObject(kind, name, namespace string) map[string]any {
	return map[string]any{"kind": kind, "metadata": map[string]any{"name": name, "namespace": namespace}}
}

// forgejoDriftDeployment is a Forgejo Deployment whose one container runs image.
func forgejoDriftDeployment(image string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "forgejo", "namespace": "forgejo"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "forgejo", "image": image}},
		}}},
	}
}

// TestCompareForgejoDrift runs the compare over one Deployment against a fake
// kubectl that answers with the live object (gt-51i8oi).
func TestCompareForgejoDrift(t *testing.T) {
	t.Parallel()
	// live is the object the cluster answers with: the same fields, plus the
	// server-set ones a manifest never carries.
	live := func(image string) string {
		obj := forgejoDriftDeployment(image)
		obj["status"] = map[string]any{"replicas": 1}
		obj["metadata"].(map[string]any)["uid"] = "0b7c"
		raw, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("encoding the live object: %v", err)
		}
		return string(raw)
	}
	answer := func(body string, exit int) func(context.Context, string, []string, string, ...string) (string, int, error) {
		return func(context.Context, string, []string, string, ...string) (string, int, error) {
			return body, exit, nil
		}
	}
	manifest := []map[string]any{forgejoDriftDeployment("forgejo:14")}
	cases := []struct {
		name      string
		objects   []map[string]any
		kubectl   func(context.Context, string, []string, string, ...string) (string, int, error)
		wantErr   []string
		wantLines int
		wantDiff  bool
		noRead    bool
	}{
		{name: "an equal live answer prints nothing", objects: manifest, kubectl: answer(live("forgejo:14"), 0)},
		{name: "a changed image prints one line", objects: manifest, kubectl: answer(live("forgejo:15"), 0), wantLines: 1, wantDiff: true},
		{name: "a failed read names the object", objects: manifest, kubectl: answer("", 1), wantErr: []string{"Deployment/forgejo", "could not be read"}},
		{name: "an absent object names the object", objects: manifest, kubectl: answer("", 0), wantErr: []string{"Deployment/forgejo", "does not hold deployment/forgejo"}},
		{name: "a live answer with no fields names the object", objects: manifest, kubectl: answer("{}", 0), wantErr: []string{"Deployment/forgejo", "no fields to compare"}},
		{name: "a refused target names the object and reads nothing", objects: []map[string]any{forgejoObject("Secret", "forgejo", "forgejo")}, kubectl: answer("", 0), wantErr: []string{"Secret/forgejo", "kind the drift compare does not read"}, noRead: true},
		{name: "no kubectl runner refuses the compare", objects: manifest, wantErr: []string{"no kubectl read runner"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			var reads int
			d := clusterDriftDeps{out: &out}
			if tc.kubectl != nil {
				d.kubectl = func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
					reads++
					return tc.kubectl(ctx, dir, env, name, args...)
				}
			}
			differed, err := compareForgejoDrift(context.Background(), d, tc.objects)
			if len(tc.wantErr) == 0 && err != nil {
				t.Fatalf("compareForgejoDrift() error = %v, want none", err)
			}
			for _, want := range tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("compareForgejoDrift() error = %v, want one naming %q", err, want)
				}
			}
			if tc.noRead && reads != 0 {
				t.Errorf("read the cluster %d times for a refused target, want none", reads)
			}
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if out.Len() == 0 {
				lines = nil
			}
			if len(lines) != tc.wantLines {
				t.Fatalf("printed %d lines %q, want %d", len(lines), out.String(), tc.wantLines)
			}
			if tc.wantLines == 1 && !strings.HasPrefix(lines[0], "deployment/forgejo: spec.template.spec.containers[0].image: rendered sha256:") {
				t.Errorf("printed %q, want the image line under deployment/forgejo", lines[0])
			}
			if differed != tc.wantDiff {
				t.Errorf("differed = %v, want %v", differed, tc.wantDiff)
			}
		})
	}
}

// TestForgejoDriftTarget checks each of the four kinds gives its resource,
// namespace, scope and object (gt-1r0cgm).
func TestForgejoDriftTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, resource, namespace string
		obj                       map[string]any
		cluster                   bool
	}{
		{"Namespace", "namespace/forgejo", "", forgejoObject("Namespace", "forgejo", ""), true},
		{"PersistentVolumeClaim", "persistentvolumeclaim/forgejo-data", "forgejo", forgejoObject("PersistentVolumeClaim", "forgejo-data", "forgejo"), false},
		{"Service", "service/forgejo", "forgejo", forgejoObject("Service", "forgejo", "forgejo"), false},
		{"Deployment", "deployment/forgejo", "forgejo", forgejoObject("Deployment", "forgejo", "forgejo"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := forgejoDriftTarget(tc.obj)
			if err != nil {
				t.Fatalf("refused a %s it should read: %v", tc.name, err)
			}
			if got.resource != tc.resource || got.namespace != tc.namespace || got.cluster != tc.cluster || !reflect.DeepEqual(got.object, tc.obj) {
				t.Errorf("got %+v, want resource %q namespace %q cluster %v over the object", got, tc.resource, tc.namespace, tc.cluster)
			}
		})
	}
}

// TestForgejoDriftTargetRefuses checks each refused object is an error naming its
// cause, with no target (gt-1r0cgm).
func TestForgejoDriftTargetRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, mention string
		obj           map[string]any
	}{
		{"no kind", "no kind", map[string]any{"metadata": map[string]any{"name": "forgejo", "namespace": "forgejo"}}},
		{"an empty kind", "no kind", forgejoObject("", "forgejo", "forgejo")},
		{"an unknown kind", `"Secret"`, forgejoObject("Secret", "forgejo", "forgejo")},
		{"no metadata", "metadata.name", map[string]any{"kind": "Service"}},
		{"an empty name", "metadata.name", forgejoObject("Service", "", "forgejo")},
		{"an empty name on a Namespace", "metadata.name", forgejoObject("Namespace", "", "")},
		{"another namespace", `namespace "default"`, forgejoObject("Deployment", "forgejo", "default")},
		{"no namespace", `namespace ""`, forgejoObject("PersistentVolumeClaim", "forgejo-data", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := forgejoDriftTarget(tc.obj)
			switch {
			case err == nil:
				t.Fatalf("read the object as %+v, want an error", got)
			case !strings.Contains(err.Error(), tc.mention):
				t.Errorf("error %q does not name %q", err, tc.mention)
			case !reflect.DeepEqual(got, driftTarget{}):
				t.Errorf("a refused object yielded target %+v, want none", got)
			}
		})
	}
}

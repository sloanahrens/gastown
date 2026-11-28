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

// forgejoDriftNamed is forgejoDriftDeployment under another name.
func forgejoDriftNamed(name, image string) map[string]any {
	obj := forgejoDriftDeployment(image)
	obj["metadata"].(map[string]any)["name"] = name
	return obj
}

// TestCompareForgejoDrift runs the compare over one Deployment against a fake
// kubectl that answers with the live object (gt-51i8oi, gt-31jbuc).
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
	const imageLine = "deployment/forgejo: spec.template.spec.containers[0].image: rendered sha256:"
	imageException := driftException{Object: "Deployment/forgejo/forgejo", Path: "spec.template.spec.containers[0].image", Reason: "pinned by the operator", Owner: "ops"}
	cases := []struct {
		name       string
		objects    []map[string]any
		exceptions []driftException
		kubectl    func(context.Context, string, []string, string, ...string) (string, int, error)
		wantErr    []string
		wantLines  int
		wantLine   string
		wantDiff   bool
		noRead     bool
	}{
		{name: "an equal live answer prints nothing", objects: manifest, kubectl: answer(live("forgejo:14"), 0)},
		{name: "a changed image prints one line", objects: manifest, kubectl: answer(live("forgejo:15"), 0), wantLines: 1, wantLine: imageLine, wantDiff: true},
		{name: "an explained image prints explained and does not set differed", objects: manifest, exceptions: []driftException{imageException}, kubectl: answer(live("forgejo:15"), 0), wantLines: 1, wantLine: "deployment/forgejo: explained: spec.template.spec.containers[0].image: rendered sha256:"},
		{name: "an exception for another object explains nothing", objects: manifest, exceptions: []driftException{{Object: "Deployment/forgejo/other", Path: imageException.Path}}, kubectl: answer(live("forgejo:15"), 0), wantLines: 1, wantLine: imageLine, wantDiff: true},
		{name: "an exception for another path explains nothing", objects: manifest, exceptions: []driftException{{Object: imageException.Object, Path: "spec.replicas"}}, kubectl: answer(live("forgejo:15"), 0), wantLines: 1, wantLine: imageLine, wantDiff: true},
		{name: "a failed read names the object", objects: manifest, kubectl: answer("", 1), wantErr: []string{"Deployment/forgejo", "could not be read"}},
		{name: "an absent object prints one line and sets differed", objects: manifest, kubectl: answer("", 0), wantLines: 1, wantLine: "deployment/forgejo: the object is absent from the cluster", wantDiff: true},
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
			differed, err := compareForgejoDrift(context.Background(), d, tc.objects, tc.exceptions)
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
			if tc.wantLine != "" && !strings.HasPrefix(lines[0], tc.wantLine) {
				t.Errorf("printed %q, want a line starting %q", lines[0], tc.wantLine)
			}
			if differed != tc.wantDiff {
				t.Errorf("differed = %v, want %v", differed, tc.wantDiff)
			}
		})
	}
}

// TestCompareForgejoDriftAbsentIsDrift: an object the cluster does not hold is one
// line and sets differed, and the object after it is still compared (gt-31jbuc).
func TestCompareForgejoDriftAbsentIsDrift(t *testing.T) {
	t.Parallel()
	changed, err := json.Marshal(forgejoDriftNamed("forgejo-proxy", "forgejo:15"))
	if err != nil {
		t.Fatalf("encoding the live object: %v", err)
	}
	objects := []map[string]any{forgejoDriftDeployment("forgejo:14"), forgejoDriftNamed("forgejo-proxy", "forgejo:14")}
	var out bytes.Buffer
	d := clusterDriftDeps{out: &out, kubectl: func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
		if args[1] == "deployment/forgejo" {
			return "", 0, nil
		}
		return string(changed), 0, nil
	}}
	differed, err := compareForgejoDrift(context.Background(), d, objects, nil)
	if err != nil {
		t.Fatalf("compareForgejoDrift() error = %v, want none", err)
	}
	if !differed {
		t.Errorf("differed = false, want true for an absent object")
	}
	want := []string{
		"deployment/forgejo: the object is absent from the cluster",
		"deployment/forgejo-proxy: spec.template.spec.containers[0].image: rendered sha256:",
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("printed %q, want %d lines", out.String(), len(want))
	}
	for i := range want {
		if !strings.HasPrefix(lines[i], want[i]) {
			t.Errorf("line %d = %q, want a line starting %q", i, lines[i], want[i])
		}
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

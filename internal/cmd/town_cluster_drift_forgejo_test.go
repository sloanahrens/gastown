package cmd

import (
	"reflect"
	"strings"
	"testing"
)

func forgejoObject(kind, name, namespace string) map[string]any {
	return map[string]any{"kind": kind, "metadata": map[string]any{"name": name, "namespace": namespace}}
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

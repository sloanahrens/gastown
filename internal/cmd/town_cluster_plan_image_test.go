package cmd

import (
	"regexp"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	updatejob "github.com/steveyegge/gastown/deploy/k8s/update-job"
)

// planImageDoc is a Deployment running image as container gt, carrying polecat
// as gastown.dev/polecat-image only when it is not empty.
func planImageDoc(polecat, image string) map[string]any {
	template := map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "gt", "image": image}}}}
	if polecat != "" {
		template["metadata"] = map[string]any{"annotations": map[string]any{k8sdaemon.PolecatImageAnnotation: polecat}}
	}
	return map[string]any{"kind": "Deployment", "spec": map[string]any{"template": template}}
}

// TestClusterShapePlanImageRollDiffers drives planImageRollDiffers over the two
// differences the drift compare skips; a missing live container is its own.
func TestClusterShapePlanImageRollDiffers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		rendered, live map[string]any
		want           bool
	}{
		{"a differing container image", planImageDoc("", "gt:rendered"), planImageDoc("", "gt:live"), true},
		{"a differing polecat-image annotation", planImageDoc("polecat:rendered", "gt:same"), planImageDoc("polecat:live", "gt:same"), true},
		{"neither differs", planImageDoc("polecat:same", "gt:same"), planImageDoc("polecat:same", "gt:same"), false},
		{"the live object lacks the container", planImageDoc("", "gt:rendered"), map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{}}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			differs, err := planImageRollDiffers(tc.rendered, tc.live)
			if err != nil || differs != tc.want {
				t.Fatalf("planImageRollDiffers() = %t, %v, want %t and no error", differs, err, tc.want)
			}
		})
	}
}

// TestClusterShapePlanImageRollShape drives planImageRollDiffers over a document
// whose shape hides an image: an error naming the key path, never a false.
func TestClusterShapePlanImageRollShape(t *testing.T) {
	t.Parallel()
	const image = "registry.example/gt:roll"
	good := planImageDoc("", image)
	podSpec := func(containers any) map[string]any {
		return map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": containers}}}}
	}
	annotations := func(value any) map[string]any {
		doc := planImageDoc("", image)
		doc["spec"].(map[string]any)["template"].(map[string]any)["metadata"] = map[string]any{"annotations": value}
		return doc
	}
	badAnnotation := map[string]any{k8sdaemon.PolecatImageAnnotation: 7}
	cases := []struct {
		name           string
		rendered, live map[string]any
	}{
		{"the pod template spec is absent", map[string]any{"spec": map[string]any{"template": map[string]any{}}}, good},
		{"the containers are not a list", podSpec("gt"), good},
		{"a container entry is not an object", podSpec([]any{"gt"}), good},
		{"a container names no container", podSpec([]any{map[string]any{"image": image}}), good},
		{"a container names no image", podSpec([]any{map[string]any{"name": "gt"}}), good},
		{"the annotations are not an object", annotations("gt"), good},
		{"the polecat-image annotation is not a string", annotations(badAnnotation), good},
		{"the live polecat-image annotation is not a string", good, annotations(badAnnotation)},
		{"the live object's shape is unreadable", good, map[string]any{"spec": image}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			differs, err := planImageRollDiffers(tc.rendered, tc.live)
			if err == nil || differs {
				t.Fatalf("planImageRollDiffers() = %t, %v, want false and an error for an unreadable document", differs, err)
			}
			if strings.Contains(err.Error(), "registry.example") {
				t.Fatalf("planImageRollDiffers() error = %q, want the key path only and no document value", err)
			}
		})
	}
}

// TestClusterShapePlanImageRollRefusal drives planImageRollRefusal: it names the
// update Job as the way to roll an image and carries no image digest (gt-aphudu).
func TestClusterShapePlanImageRollRefusal(t *testing.T) {
	t.Parallel()
	err := planImageRollRefusal()
	if err == nil {
		t.Fatal("planImageRollRefusal() = nil, want the refusal")
	}
	message := err.Error()
	for _, want := range []string{updatejob.Name + " Job", "Role " + updateRoleName} {
		if !strings.Contains(message, want) {
			t.Fatalf("planImageRollRefusal() = %q, want it to name %q", message, want)
		}
	}
	if regexp.MustCompile(`\b[0-9a-f]{64}\b`).MatchString(message) || strings.Contains(message, "sha256:") {
		t.Fatalf("planImageRollRefusal() = %q, want no image digest", message)
	}
}

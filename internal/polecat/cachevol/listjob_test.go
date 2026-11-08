package cachevol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// listInput is an input that builds: the tests vary one field at a time.
func listInput() ListJobInput {
	return ListJobInput{
		Namespace:    "gt",
		Image:        "gastown-polecat:dev",
		ClaimName:    "gt-cache",
		NodeSelector: map[string]string{"pool": "sandbox"},
		Tolerations:  []Toleration{{Key: "sandbox", Operator: "Exists", Effect: "NoSchedule"}},
	}
}

// TestBuildListManifestGolden pins the rendered lister Job: it runs trim-list on the claim, mounts
// the claim read-only and gives the Job no retry.
func TestBuildListManifestGolden(t *testing.T) {
	t.Parallel()
	golden, err := os.ReadFile(filepath.Join("testdata", "listjob.json"))
	if err != nil {
		t.Fatalf("reading the golden manifest: %v", err)
	}
	m, err := BuildListManifest(listInput())
	if err != nil {
		t.Fatalf("BuildListManifest: %v", err)
	}
	docs := m.Documents()
	if len(docs) != 1 {
		t.Fatalf("the manifest renders %d documents, want the one Job", len(docs))
	}
	if got := string(docs[0]); got != string(golden) {
		t.Errorf("the lister Job changed:\n--- got ---\n%s\n--- want ---\n%s", got, golden)
	}
}

// trimRefusal is BuildTrimManifest's refusal for the name, namespace, image and claim in in, with
// the rest of the input building. It fails the test when the trim builds, since then the case does
// not compare a refusal.
func trimRefusal(t *testing.T, in ListJobInput) error {
	t.Helper()
	ti := trimInput()
	ti.Name = in.Name
	ti.Namespace = in.Namespace
	ti.Image = in.Image
	ti.ClaimName = in.ClaimName
	_, err := BuildTrimManifest(ti)
	if err == nil {
		t.Fatalf("BuildTrimManifest(%+v) builds, so the case does not compare a refusal", ti)
	}
	return err
}

// TestBuildListManifestRefusesBadInput pins that the lister Job refuses an input with the message
// BuildTrimManifest gives for the same input.
func TestBuildListManifestRefusesBadInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*ListJobInput)
	}{
		{"a name that is not a label", func(in *ListJobInput) { in.Name = "gt-cache-list/1" }},
		{"a namespace that is not a label", func(in *ListJobInput) { in.Namespace = "gt cache" }},
		{"no image", func(in *ListJobInput) { in.Image = "" }},
		{"an image with a space", func(in *ListJobInput) { in.Image = "gastown polecat:dev" }},
		{"no claim", func(in *ListJobInput) { in.ClaimName = "" }},
		{"a claim that is not a label", func(in *ListJobInput) { in.ClaimName = "gt_cache" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := listInput()
			tc.edit(&in)
			_, err := BuildListManifest(in)
			if err == nil {
				t.Fatalf("BuildListManifest(%+v) builds, want a refusal", in)
			}
			if want := trimRefusal(t, in); err.Error() != want.Error() {
				t.Errorf("BuildListManifest refused with %q, want the trim's %q", err, want)
			}
		})
	}
}

// TestBuildListManifestMountsReadOnly pins the claim read-only in the Job kubectl applies: the volume
// and the mount both say so, so a writable setting in either place fails it.
func TestBuildListManifestMountsReadOnly(t *testing.T) {
	t.Parallel()
	m, err := BuildListManifest(listInput())
	if err != nil {
		t.Fatalf("BuildListManifest: %v", err)
	}
	var job SeedJob
	if err := json.Unmarshal(m.Documents()[0], &job); err != nil {
		t.Fatalf("the document is not a Job: %v", err)
	}
	spec := job.Spec.Template.Spec
	if len(spec.Volumes) != 1 || spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatalf("the Job's volumes are %+v, want the claim alone", spec.Volumes)
	}
	if !spec.Volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Errorf("the claim volume is writable, want readOnly")
	}
	mounts := spec.Containers[0].VolumeMounts
	if len(mounts) != 1 || !mounts[0].ReadOnly {
		t.Errorf("the container mounts %+v, want the claim read-only", mounts)
	}
}

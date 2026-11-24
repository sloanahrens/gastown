package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// clusterControlObjectNames are files 01 to 11 of the control render, in apply
// order: the namespace, the claims, the service account, the Forgejo Service, and
// the role with its bindings. The Deployment (12) is not in this golden.
var clusterControlObjectNames = []string{
	"01-namespace-gt.yaml",
	"02-persistentvolumeclaim-town-state.yaml",
	"03-persistentvolumeclaim-landing-work.yaml",
	"04-persistentvolumeclaim-dolt-backup.yaml",
	"05-serviceaccount-daemon.yaml",
	"06-service-forgejo.yaml",
	"07-role-deployer-secret-names.yaml",
	"08-rolebinding-deployer-secret-names.yaml",
	"09-rolebinding-daemon-sweep-runner.yaml",
	"10-rolebinding-daemon-gt-ci-cache-trim.yaml",
	"11-clusterrolebinding-daemon-gt-sweep-reader.yaml",
}

// renderedFiles runs the render of f and returns the named files as "=== <name> ==="
// then the file's bytes, in the order named. A named file the render did not write
// fails the test.
func renderedFiles(t *testing.T, f clusterRenderFlags, names []string) string {
	t.Helper()
	if err := runClusterRender(clusterRenderTestDeps(t, io.Discard), f); err != nil {
		t.Fatalf("render: %v", err)
	}
	var got strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(f.out, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		fmt.Fprintf(&got, "=== %s ===\n%s", name, data)
	}
	return got.String()
}

// The control render's files 01 to 11 are pinned byte for byte. The render must
// write them under these names, first in its listing, so a renamed, added or
// reordered file fails here as well as a changed body.
func TestTownClusterRenderControlGoldenObjects(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	got := renderedFiles(t, f, clusterControlObjectNames)
	written := dirNames(t, f.out)
	n := len(clusterControlObjectNames)
	if len(written) < n || !slices.Equal(written[:n], clusterControlObjectNames) {
		t.Fatalf("the control render's first %d files are %q, want %q", n, written, clusterControlObjectNames)
	}
	if err := compareGolden(filepath.Join("testdata", "cluster_render_control_objects.golden"), got); err != nil {
		t.Fatal(err)
	}
}

// The control render's Deployment (file 12) is pinned byte for byte, under the
// name the render writes it at, so a change to any of its fields fails here.
func TestTownClusterRenderControlGoldenDeployment(t *testing.T) {
	t.Parallel()
	f := clusterRenderFixture(t)
	got := renderedFiles(t, f, []string{"12-deployment-daemon.yaml"})
	if err := compareGolden(filepath.Join("testdata", "cluster_render_control_deployment.golden"), got); err != nil {
		t.Fatal(err)
	}
}

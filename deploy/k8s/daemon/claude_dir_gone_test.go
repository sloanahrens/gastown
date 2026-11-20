package daemon

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retiredClaudeDirs are the two volumes and mounts the render retired: the Mac's
// ~/.claude carried into the dashboard and the daemon as a hostPath (gt-thh0bs).
// The render reads its cost and usage rows from Dolt, so neither pod names a host
// directory.
var retiredClaudeDirs = struct {
	volumes   []string
	mountPath []string
}{
	volumes:   []string{"dashboard-claude", "daemon-claude"},
	mountPath: []string{"/root/.claude", "/root/.claude-host/projects"},
}

// TestClaudeDirGoneRender holds the rendered daemon and dashboard Deployments to
// their golden files, and to the absence of the two retired Mac ~/.claude
// hostPath volumes: no volume or mount in either pod carries a claude directory
// (gt-thh0bs).
func TestClaudeDirGoneRender(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(packageDir(t), "testdata")

	daemon := mustBuild(t, goldenValues(t))
	compareGolden(t, dir, "deployment.yaml", document(t, daemon.Deployment))
	assertNoClaudeDirMount(t, "daemon", daemon.Deployment.Spec.Template.Spec)

	dashboard := mustBuild(t, dashboardValues(t))
	compareGolden(t, dir, "deployment-dashboard.yaml", document(t, dashboard.DashboardDeployment))
	assertNoClaudeDirMount(t, "dashboard", dashboard.DashboardDeployment.Spec.Template.Spec)
}

// assertNoClaudeDirMount fails when the pod names a retired claude volume, or a
// container mounts a retired claude path. It reads every container, so a mount
// added beside the daemon's own container is caught too.
func assertNoClaudeDirMount(t *testing.T, what string, spec PodSpec) {
	t.Helper()
	for _, vol := range spec.Volumes {
		for _, name := range retiredClaudeDirs.volumes {
			if vol.Name == name {
				t.Errorf("the %s pod carries the retired claude volume %q: %+v", what, name, vol)
			}
		}
	}
	for _, ctr := range spec.Containers {
		for _, mount := range ctr.VolumeMounts {
			for _, name := range retiredClaudeDirs.volumes {
				if mount.Name == name {
					t.Errorf("the %s container %q mounts the retired volume %q: %+v", what, ctr.Name, name, mount)
				}
			}
			for _, retired := range retiredClaudeDirs.mountPath {
				if mount.MountPath == retired || strings.HasPrefix(mount.MountPath, retired+"/") {
					t.Errorf("the %s container %q mounts the retired claude path %q: %+v", what, ctr.Name, mount.MountPath, mount)
				}
			}
		}
	}
}

// TestClaudeDirGoneGuard holds the two retired flags to their one remaining
// mention: the refusal in internal/cmd/town_cluster_render.go that tells an
// operator why the flag is gone. No other non-test Go source under internal/ or
// deploy/ names either flag (gt-thh0bs).
func TestClaudeDirGoneGuard(t *testing.T) {
	t.Parallel()
	root := filepath.Join(packageDir(t), "..", "..", "..")
	const allowed = "internal/cmd/town_cluster_render.go"
	retiredFlags := []string{"dashboard-claude-dir", "daemon-claude-dir"}

	// The exempt file must still name both flags, or this guard would pass while
	// the refusal it protects had gone, and the exemption would be dead text.
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(allowed)))
	if err != nil {
		t.Fatalf("reading the refusal file %s: %v", allowed, err)
	}
	for _, flag := range retiredFlags {
		if !bytes.Contains(src, []byte(flag)) {
			t.Fatalf("%s names no %s: the refusal this guard exempts is gone, so the guard would pass for the wrong reason", allowed, flag)
		}
	}

	scanned := 0
	for _, top := range []string{"internal", "deploy"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == allowed {
				return nil
			}
			scanned++
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, flag := range retiredFlags {
				if bytes.Contains(data, []byte(flag)) {
					t.Errorf("%s names the retired flag %s, want only %s to", rel, flag, allowed)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", top, err)
		}
	}
	if scanned == 0 {
		t.Fatalf("the guard read no non-test Go source under internal/ or deploy/, so the scan looked at nothing")
	}
}

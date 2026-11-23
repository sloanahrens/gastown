package daemon

import (
	"bytes"
	"fmt"
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

// retiredFlagViolations walks the non-test Go files under the tops beneath root,
// leaving out allowed, and returns one line for each file that names a retired
// flag. scanned is how many files it read, so a caller can tell a clean tree
// from one the walk never reached.
func retiredFlagViolations(root string, tops, flags []string, allowed string) (violations []string, scanned int, err error) {
	for _, top := range tops {
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
			for _, flag := range flags {
				if bytes.Contains(data, []byte(flag)) {
					violations = append(violations, fmt.Sprintf("%s names the retired flag %s", rel, flag))
				}
			}
			return nil
		})
		if err != nil {
			return nil, scanned, fmt.Errorf("walking %s: %w", top, err)
		}
	}
	return violations, scanned, nil
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

	violations, scanned, err := retiredFlagViolations(root, []string{"internal", "deploy"}, retiredFlags, allowed)
	if err != nil {
		t.Fatalf("scanning the source: %v", err)
	}
	for _, v := range violations {
		t.Errorf("%s, want only %s to", v, allowed)
	}
	if scanned == 0 {
		t.Fatalf("the guard read no non-test Go source under internal/ or deploy/, so the scan looked at nothing")
	}
}

// TestClaudeDirGoneGuardReportsAViolation drives the guard's failing branch: a
// non-test Go file beside the exemption that names a retired flag is reported,
// while the exempt file and a _test.go naming one are not (gt-f9pens).
func TestClaudeDirGoneGuardReportsAViolation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatalf("creating internal: %v", err)
	}
	for rel, body := range map[string]string{
		"internal/refusal.go":      "refuse dashboard-claude-dir",
		"internal/other.go":        "// daemon-claude-dir is retired",
		"internal/ignored_test.go": "// dashboard-claude-dir",
	} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", rel, err)
		}
	}
	violations, scanned, err := retiredFlagViolations(root, []string{"internal"}, []string{"dashboard-claude-dir", "daemon-claude-dir"}, "internal/refusal.go")
	if err != nil {
		t.Fatalf("scanning the built tree: %v", err)
	}
	if want := "internal/other.go names the retired flag daemon-claude-dir"; len(violations) != 1 || violations[0] != want || scanned != 1 {
		t.Errorf("violations = %q, scanned = %d, want [%q] and 1 file read", violations, scanned, want)
	}
}

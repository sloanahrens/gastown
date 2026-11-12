package templates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// writeProbeEnv sets the value of GT_PROBE_MARK in the town's daemon.env, which
// the rendered plist carries as an EnvironmentVariables entry.
func writeProbeEnv(t *testing.T, town, value string) {
	t.Helper()
	envPath := config.DaemonEnvPath(town)
	if err := os.MkdirAll(filepath.Dir(envPath), 0755); err != nil {
		t.Fatalf("creating settings dir: %v", err)
	}
	if err := os.WriteFile(envPath, []byte("GT_PROBE_MARK="+value+"\n"), 0644); err != nil {
		t.Fatalf("writing daemon.env: %v", err)
	}
}

// A darwin provision writes the plist and reports that launchd loads it at the
// next login. It does not report a load, because gt does not load it, and a
// re-provision over a plist launchd may already have loaded says the same.
func TestProvisionSupervisor_DarwinReportsWriteNotLoad(t *testing.T) {
	t.Parallel()
	h := fakeHost(t, "darwin")
	town := t.TempDir()
	plistPath, kind := h.filePath()
	if kind != "launchd" {
		t.Fatalf("filePath() kind = %q, want launchd", kind)
	}
	want := "Wrote launchd plist " + plistPath + "; launchd loads it at the next login"

	for _, pass := range []string{"first", "re-provision"} {
		got, err := h.provisionSupervisor(town, 30*time.Second)
		if err != nil {
			t.Fatalf("%s provision: %v", pass, err)
		}
		if got != want {
			t.Errorf("%s provision message = %q, want %q", pass, got, want)
		}
	}
}

// A re-provision rewrites the plist that is already on disk from the town's
// current state, and does not keep the old file.
func TestProvisionSupervisor_DarwinReprovisionRewritesPlist(t *testing.T) {
	t.Parallel()
	h := fakeHost(t, "darwin")
	town := t.TempDir()
	plistPath, _ := h.filePath()

	writeProbeEnv(t, town, "first")
	if _, err := h.provisionSupervisor(town, 30*time.Second); err != nil {
		t.Fatalf("first provision: %v", err)
	}

	writeProbeEnv(t, town, "second")
	if _, err := h.provisionSupervisor(town, 30*time.Second); err != nil {
		t.Fatalf("re-provision: %v", err)
	}

	body, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("reading plist after re-provision: %v", err)
	}
	if !strings.Contains(string(body), "<string>second</string>") {
		t.Errorf("plist after re-provision does not carry the new GT_PROBE_MARK value:\n%s", body)
	}
	if strings.Contains(string(body), "<string>first</string>") {
		t.Errorf("plist after re-provision still carries the old GT_PROBE_MARK value:\n%s", body)
	}
}

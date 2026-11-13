package templates

import (
	"os"
	"testing"
)

// A darwin provision writes no supervisor file: gt no longer provisions a
// launchd job, and a macOS town runs its daemon in the container. It reports
// the same unsupported-host message as any other host with no supervisor gt
// configures, and creates nothing at the plist path it would have used.
func TestProvisionSupervisor_DarwinWritesNothing(t *testing.T) {
	t.Parallel()
	h := fakeHost(t, "darwin")
	town := t.TempDir()
	plistPath, _ := h.filePath()

	got, err := h.provisionSupervisor(town)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	want := "Supervisor auto-configuration skipped on darwin (not supported yet)"
	if got != want {
		t.Errorf("provision message = %q, want %q", got, want)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("darwin provision left a file at %s (stat err = %v), want none", plistPath, err)
	}
}

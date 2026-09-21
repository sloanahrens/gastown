package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// Both harness entry points must point test-spawned bd at their OWN circuit
// directory (gt-tkz7): StartHermetic at the suite sandbox, HermeticTest at
// the per-test sandbox — not at an outer suite path its scrub left behind.
// CleanGTEnv passes BEADS_* through unchanged, so subprocesses need nothing
// more.
func TestHermetic_RedirectsBDCircuitDirPerSandbox(t *testing.T) {
	withSavedEnv(t)
	_ = os.Unsetenv(BeadsCircuitDirEnv)

	h, err := StartHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	defer h.Finish(0)

	suiteDir := filepath.Join(h.HomeDir, ".cache", "beads-circuit")
	if got := os.Getenv(BeadsCircuitDirEnv); got != suiteDir {
		t.Fatalf("after StartHermetic %s = %q, want %q", BeadsCircuitDirEnv, got, suiteDir)
	}
	if !filepath.IsAbs(suiteDir) {
		t.Fatalf("circuit dir must be absolute for bd to honour it: %q", suiteDir)
	}

	// HermeticTest scrubs and builds its own sandbox: the circuit dir must
	// follow that sandbox's HOME, not survive as the suite's path.
	HermeticTest(t)
	testDir := filepath.Join(os.Getenv("HOME"), ".cache", "beads-circuit")
	got := os.Getenv(BeadsCircuitDirEnv)
	if got != testDir {
		t.Errorf("after HermeticTest %s = %q, want %q (this sandbox)", BeadsCircuitDirEnv, got, testDir)
	}
	if got == suiteDir {
		t.Errorf("HermeticTest kept the outer suite circuit dir %q", suiteDir)
	}
}

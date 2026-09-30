package testutil

import (
	"path/filepath"
	"testing"
)

// Both harness entry points must point test-spawned bd at their OWN circuit
// directory (gt-tkz7): StartHermetic at the suite sandbox, HermeticTest at
// the per-test sandbox — not at an outer suite path its scrub left behind.
// CleanGTEnv passes BEADS_* through unchanged, so subprocesses need nothing
// more.
func TestHermetic_RedirectsBDCircuitDirPerSandbox(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, goEnvSet...)
	h, err := f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	defer h.Finish(0)
	suiteDir := filepath.Join(h.HomeDir, ".cache", "beads-circuit")
	if got := f.env.get(BeadsCircuitDirEnv); got != suiteDir {
		t.Fatalf("after StartHermetic %s = %q, want %q", BeadsCircuitDirEnv, got, suiteDir)
	}
	if !filepath.IsAbs(suiteDir) {
		t.Fatalf("circuit dir must be absolute for bd to honour it: %q", suiteDir)
	}
	// HermeticTest scrubs and builds its own sandbox: the circuit dir must
	// follow that sandbox's HOME, not survive as the suite's path.
	f.hermeticTest(t)
	testDir := filepath.Join(f.env.get("HOME"), ".cache", "beads-circuit")
	got := f.env.get(BeadsCircuitDirEnv)
	if got != testDir {
		t.Errorf("after HermeticTest %s = %q, want %q (this sandbox)", BeadsCircuitDirEnv, got, testDir)
	}
	if got == suiteDir {
		t.Errorf("HermeticTest kept the outer suite circuit dir %q", suiteDir)
	}
}

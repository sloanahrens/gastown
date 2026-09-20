package refinery

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, Dolt
// ports poisoned so nothing reaches the production server, and a tripwire
// that fails the run if any state leaks into a live town. Tests that need a
// Dolt server start the shared container lazily via RequireDoltContainer;
// Finish terminates it.
func TestMain(m *testing.M) {
	// One session-prefix registry for the whole package: "testrig" → "xut",
	// a prefix no real rig uses (the natural "tr" collides with rigs on the
	// host, so tests asserting "no session exists" failed in workspaces).
	// It used to be swapped in per test with a Cleanup restore; with the
	// Manager tests now t.Parallel, one test's restore would pull the prefix
	// from under the others (gt-yfom). Unregistered rigs still resolve to
	// DefaultPrefix, exactly as the empty hermetic registry did.
	reg := session.NewPrefixRegistry()
	reg.Register("xut", "testrig")
	session.SetDefaultRegistry(reg)
	os.Exit(testutil.HermeticMain(m))
}

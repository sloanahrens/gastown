package refinery

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, Dolt
// ports poisoned so nothing reaches the production server, and a tripwire
// that fails the run if any state leaks into a live town. Tests that need a
// Dolt server start the shared container lazily via RequireDoltContainer;
// Finish terminates it.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

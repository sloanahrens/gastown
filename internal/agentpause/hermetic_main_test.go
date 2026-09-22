package agentpause

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, Dolt
// ports poisoned so nothing reaches the production server, and a tripwire
// that fails the run if any state leaks into a live town.
//
// The package writes to $HOME/.runtime — plain os.TempDir() sandboxing isn't
// enough to keep that off a live town, so it opts into the harness even
// though it no longer imports the beads SDK (gt-ahik: the marker file is the
// only source of truth, so there is no bead layer left to fake).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

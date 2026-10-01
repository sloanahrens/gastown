package beads_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, Dolt
// ports poisoned so nothing reaches the production server, and workspace
// resolution refusing the live town. The unit tier still starts bd, sh and
// sleep (testutil.AllowTools, a baseline that only shrinks).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.AllowTools("bd", "sh", "sleep")))
}

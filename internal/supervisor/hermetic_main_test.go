package supervisor

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, Dolt
// ports poisoned so nothing reaches the production server, and workspace
// resolution refusing the live town.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

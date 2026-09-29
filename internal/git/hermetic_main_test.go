package git

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, Dolt
// ports poisoned so nothing reaches the production server, and a tripwire
// that fails the run if any state leaks into a live town. The sandbox
// gitconfig turns off git's fsync (gt-22hdp.33).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

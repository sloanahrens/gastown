package feed_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, Dolt
// ports poisoned so nothing reaches the production server, and a tripwire
// that fails the run if any state leaks into a live town. Git on PATH refuses
// to run (internal/testpolicy/gitfree.txt): nothing here needs it. It is in
// the external test package because testutil imports feed.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}

package deliver

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness: GT_*/BD_*
// env scrubbed, HOME and town root redirected to a sandbox, and git on PATH
// refusing to run (internal/testpolicy/gitfree.txt): nothing here needs it.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}

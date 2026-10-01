package hooks_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, and
// workspace resolution refusing the live town. Tests name their config home as
// a configHome instead of setting HOME. WithoutGit puts a refusing git first
// on PATH (internal/testpolicy/gitfree.txt). It lives in the external test
// package because testutil imports hooks (through beads and runtime).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}

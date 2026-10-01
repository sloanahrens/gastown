//go:build !integration

package doctor

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the unit tier under the hermetic harness (gt-lwi) with a
// refusing git on PATH (internal/testpolicy/gitfree.txt): the checks reach
// git through CheckContext.openGit, which the tests point at gitfake. The
// harness scrubs GT_*/BD_* env, redirects HOME and the town root to a
// sandbox, poisons the Dolt ports and fails the run if state leaks into a
// live town. The unit tier still starts diskutil, find and which
// (testutil.AllowTools, a baseline that only shrinks).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit(), testutil.AllowTools("diskutil", "find", "which")))
}

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
// live town. The unit tier starts no external tool: the checks that do in
// production take seams (DiskSpaceCheck.diskSpace, PrimingCheck.lookPath).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}

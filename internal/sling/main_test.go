package sling

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the unit tier under the hermetic harness with no container: the
// engine reaches outside the process only through Deps, and the tests here are
// the ones that need no collaborator at all. Git on PATH refuses to run
// (internal/testpolicy/gitfree.txt), which is what a dispatch that never shells
// out should see.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}

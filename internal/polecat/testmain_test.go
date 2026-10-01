//go:build !integration

package polecat

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's unit tier under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, Dolt
// ports poisoned so nothing reaches the production server, and workspace
// resolution refusing the live town. WithoutGit puts a refusing git first on
// PATH: the unit tier answers git through gitfake
// (internal/testpolicy/gitfree.txt). The integration tier's TestMain is in
// testmain_integration_test.go.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}

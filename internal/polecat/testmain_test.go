//go:build !integration

package polecat

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's unit tier under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, Dolt
// ports poisoned so nothing reaches the production server, and a tripwire
// that fails the run if any state leaks into a live town. WithoutGit puts a
// refusing git first on PATH: the unit tier answers git through gitfake
// (internal/testpolicy/gitfree.txt). The integration tier's TestMain is in
// testmain_integration_test.go.
func TestMain(m *testing.M) {
	// Every test sees the same prefix registry, set once here rather than
	// swapped per test, so the tests can run in parallel.
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	reg.Register("bd", "beads")
	reg.Register("gm", "gastown_manager")
	reg.Register("tr", "testrig")
	session.SetDefaultRegistry(reg)
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}

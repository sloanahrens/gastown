//go:build !integration

package convoy

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the unit tier under the hermetic harness with no container:
// every beads store a unit test touches is an in-memory one (fakeRigStore),
// and bd and gt calls go to in-process scripts (bdScript, gtScript,
// slingLog). Git on PATH refuses to run (internal/testpolicy/gitfree.txt).
// The Dolt-backed test lives in the integration tier, whose TestMain
// (testmain_integration_test.go) starts the container.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}

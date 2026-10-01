//go:build !integration

package daemon

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the unit tier under the hermetic harness with no container:
// every beads store a unit test touches is an in-memory one (memStore,
// beadsfake). The container-backed tests live in the integration tier, whose
// TestMain (testmain_integration_test.go) starts the Dolt container.
//
// The unit tier still starts gt, and lsof, ps and ss through the Dolt server
// manager's port and process probes (testutil.AllowTools, a baseline that
// only shrinks).
func TestMain(m *testing.M) {
	os.Exit(runDaemonTests(m, nil, testutil.WithoutGit(), testutil.AllowTools("gt", "lsof", "ps", "ss")))
}

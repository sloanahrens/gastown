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
func TestMain(m *testing.M) {
	os.Exit(runDaemonTests(m, nil, testutil.WithoutGit()))
}

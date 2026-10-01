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
// The unit tier starts no external tool: Dolt server manager tests answer its
// probes and alert mail through the manager's seams (identityCheckFn,
// notifier and the rest), and the harness refuses any tool they miss.
func TestMain(m *testing.M) {
	os.Exit(runDaemonTests(m, nil, testutil.WithoutGit()))
}

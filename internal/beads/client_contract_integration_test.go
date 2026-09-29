//go:build integration

package beads_test

import (
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestIntegrationClientContract runs the Client and Admin contracts against
// bd on databases from the shared test Dolt container's pool, through the exported
// constructor. It needs GT_TEST_DOCKER=1 and Docker (run it under gt slot
// run); without them it fails rather than skips.
func TestIntegrationClientContract(t *testing.T) {
	if !testutil.DockerTestsEnabled() {
		t.Fatal("TestIntegrationClientContract needs the Dolt test container: run under gt slot run with GT_TEST_DOCKER=1")
	}
	if err := testutil.EnsureDoltContainerForTestMain(); err != nil {
		t.Fatalf("Dolt test container: %v", err)
	}
	port, err := strconv.Atoi(testutil.DoltContainerPort())
	if err != nil || port == 0 {
		t.Fatalf("no Dolt test container (port %q): %v", testutil.DoltContainerPort(), err)
	}
	// One database per contract: the client cases share theirs (each asserts
	// only on its own issues), the admin cases run in turn on theirs. Two bd
	// inits a run instead of one per case.
	newDB := func(t *testing.T, dir string) *beads.Beads {
		b := beads.NewIsolatedWithPort(dir, port)
		if err := b.Init("gt"); err != nil {
			t.Fatalf("bd init on the test container: %v", err)
		}
		return b
	}
	t.Run("client", func(t *testing.T) {
		t.Parallel()
		shared := newDB(t, t.TempDir())
		beadsfake.RunClientContract(t, func(*testing.T) beads.Client { return shared })
	})
	t.Run("admin", func(t *testing.T) {
		t.Parallel()
		beadsfake.RunAdminContract(t, func(t *testing.T) beadsfake.AdminClient { return newDB(t, t.TempDir()) })
	})
}

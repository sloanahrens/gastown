//go:build integration

package beads_test

import (
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestIntegrationClientContract runs the Client contract against bd on a
// database from the shared test Dolt container's pool, through the exported
// constructor. It needs GT_TEST_DOCKER=1 and Docker (run it under gt slot
// run); without them it fails rather than skips.
func TestIntegrationClientContract(t *testing.T) {
	if !testutil.DockerTestsEnabled() {
		t.Fatal("TestIntegrationClientContract needs the Dolt test container: run under gt slot run with GT_TEST_DOCKER=1")
	}
	t.Run("container", func(t *testing.T) { testutil.RequireDoltContainer(t) })
	port, err := strconv.Atoi(testutil.DoltContainerPort())
	if err != nil || port == 0 {
		t.Fatalf("no Dolt test container (port %q): %v", testutil.DoltContainerPort(), err)
	}
	beadsfake.RunClientContract(t, func(t *testing.T) beads.Client {
		b := beads.NewIsolatedWithPort(t.TempDir(), port)
		if err := b.Init("gt"); err != nil {
			t.Fatalf("bd init on the test container: %v", err)
		}
		return b
	})
}

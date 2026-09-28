//go:build integration

package notify_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness with the
// package's own Dolt container: the contract runs a real gt, which must never
// see the caller's town identity or reach a live Dolt server, and pins its
// successful sends by what they leave in a town's beads. Without the
// container the tier fails; it does not pass vacuously.
func TestMain(m *testing.M) {
	h, err := testutil.StartHermetic(testutil.WithDolt())
	if err != nil {
		fmt.Fprintf(os.Stderr, "notify integration TestMain: %v\n", err)
		os.Exit(1)
	}
	if testutil.DoltContainerPort() == "" {
		fmt.Fprintln(os.Stderr, "notify integration TestMain: no Dolt container — run with GT_TEST_DOCKER=1 under gt slot run")
		os.Exit(h.Finish(1))
	}
	os.Exit(h.Finish(m.Run()))
}

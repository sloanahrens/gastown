package convoy

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi)
// with an eager Dolt container: setupTestStore sets BEADS_TEST_MODE=1, which
// causes the beads SDK to create testdb_<hash> databases. Routing those to an
// isolated container (via BEADS_DOLT_PORT) means they are destroyed when the
// container is terminated at cleanup, instead of accumulating as orphans in
// the shared production Dolt data dir.
//
// When Docker is unavailable the whole package is skipped, matching the
// pre-harness behavior (every test here needs the store).
func TestMain(m *testing.M) {
	h, err := testutil.StartHermetic(testutil.WithDolt())
	if err != nil {
		fmt.Fprintf(os.Stderr, "convoy TestMain: %v\n", err)
		os.Exit(1)
	}
	if testutil.DoltContainerPort() == "" {
		fmt.Fprintln(os.Stderr, "convoy TestMain: skipping — Dolt container unavailable")
		os.Exit(h.Finish(0))
	}
	os.Exit(h.Finish(m.Run()))
}

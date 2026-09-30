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
// Without a container (make gate runs with GT_TEST_DOCKER=0) the store tests
// skip themselves through testutil.OpenTestStore; the tests that use a fake
// bd on PATH need no container and still run.
func TestMain(m *testing.M) {
	h, err := testutil.StartHermetic(testutil.WithDolt())
	if err != nil {
		fmt.Fprintf(os.Stderr, "convoy TestMain: %v\n", err)
		os.Exit(1)
	}
	os.Exit(h.Finish(m.Run()))
}

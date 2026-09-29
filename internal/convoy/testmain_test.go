package convoy

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi)
// with an eager Dolt container: BEADS_TEST_MODE=1 causes the beads SDK to
// create testdb_<hash> databases. Routing those to an isolated container
// (via BEADS_DOLT_PORT) means they are destroyed when the container is
// terminated at cleanup, instead of accumulating as orphans in the shared
// production Dolt data dir.
//
// BEADS_TEST_MODE is set here, once, rather than by each test: StartHermetic
// scrubs every BEADS_* variable from the process environment as part of its
// setup (internal/testutil scrubProcessEnv), so setting it before that call
// would just be wiped. It is set after the scrub and before m.Run(), so it is
// in place for every test without any test calling t.Setenv — a per-test
// Setenv would panic when combined with t.Parallel() ("t.Setenv ... can not
// use t.Parallel"), and this package's tests need both the real store and
// real concurrency.
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
	os.Setenv("BEADS_TEST_MODE", "1") //nolint:tenv // intentional process-wide env, see comment above
	os.Exit(h.Finish(m.Run()))
}

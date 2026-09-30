//go:build integration

package daemon

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness with an
// ephemeral Dolt container (WithDolt), which also creates, before any test
// runs, the pool of databases the store tests open (testutil/doltpool.go):
// nothing creates or drops a database while tests run. Container tests are
// opt-in (GT_TEST_DOCKER=1, as make test-integration sets); without it the
// store tests skip, and once opted in a store that cannot open fails.
//
// BEADS_TEST_MODE=1 makes the beads SDK name its database testdb_<hash> of
// the store path, which is how a pooled path finds its database. It is set
// once here rather than per test so the store tests stay eligible for
// t.Parallel (gt-fx3c): t.Setenv panics inside a parallel test.
func TestMain(m *testing.M) {
	// Signal-target helper (pid_identity_integration_test.go): this binary
	// re-executed under a chosen argv0/argv so a test owns a process that
	// looks like `gt daemon run` or `dolt sql-server`, and can prove the stop
	// paths signal it — without any test ever pointing a stop path at a host
	// PID.
	if os.Getenv(signalTargetHelperEnv) == "1" {
		runSignalTargetHelper()
	}
	setup := func() { _ = os.Setenv("BEADS_TEST_MODE", "1") }
	os.Exit(runDaemonTests(m, setup, testutil.WithDolt()))
}

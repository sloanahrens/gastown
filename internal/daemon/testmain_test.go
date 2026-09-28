package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
	"github.com/steveyegge/gastown/internal/tmux"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, and a
// tripwire that fails the run if state leaks into a live town.
//
// WithDolt starts an ephemeral Dolt container for this package's tests and
// creates, before any test runs, the pool of databases the store tests open
// (testutil/doltpool.go): nothing creates or drops a database while tests run.
// BEADS_TEST_MODE=1 below makes the beads SDK name its database testdb_<hash>
// of the store path, which is how a pooled path finds its database. Every
// database goes with the container at cleanup.
//
// Container tests are opt-in (GT_TEST_DOCKER=1, as make test sets). Without
// it, or without Docker, setupTestStore skips via testutil.OpenTestStore;
// once opted in, a store that cannot open fails the test. Non-Dolt tests
// (e.g. boot_spawn_frequency_test.go) still run. (fixes gt-kw4449)
func TestMain(m *testing.M) {
	// Signal-target helper (pid_identity_test.go): this binary re-executed
	// under a chosen argv0/argv so a test owns a process that looks like
	// `gt daemon run` or `dolt sql-server`, and can prove the stop paths
	// signal it — without any test ever pointing a stop path at a host PID.
	if os.Getenv(signalTargetHelperEnv) == "1" {
		runSignalTargetHelper()
	}

	h, err := testutil.StartHermetic(testutil.WithDolt())
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon TestMain: %v\n", err)
		os.Exit(1)
	}

	// BEADS_TEST_MODE is process-wide rather than per-test-setenv so the
	// store-backed tests stay eligible for t.Parallel (gt-fx3c): t.Setenv
	// panics inside a parallel test, which pinned all 32 setupTestStore
	// callers to serial execution. Setting it once here is sufficient
	// because no daemon test calls testutil.HermeticTest, the only thing
	// that re-scrubs BEADS_* mid-run.
	_ = os.Setenv("BEADS_TEST_MODE", "1")

	// Isolate tmux sessions on a package-specific socket.
	// handler_test.go creates tmux.NewTmux() instances that query has-session;
	// polecat_health_test.go uses fake tmux stubs but still constructs Tmux
	// instances. Routing all of these to an isolated socket prevents
	// interference with the user's tmux and other packages' tests.
	var tmuxSocket string
	if _, err := exec.LookPath("tmux"); err == nil {
		tmuxSocket = fmt.Sprintf("gt-test-daemon-%d", os.Getpid())
		tmux.SetDefaultSocket(tmuxSocket)
	}

	code := m.Run()

	if tmuxSocket != "" {
		_ = exec.Command("tmux", "-L", tmuxSocket, "kill-server").Run()
		socketPath := filepath.Join(tmux.SocketDir(), tmuxSocket)
		_ = os.Remove(socketPath)
	}
	os.Exit(h.Finish(code))
}

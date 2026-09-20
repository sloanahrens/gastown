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
// WithDolt starts an ephemeral Dolt container for this package's tests.
// BEADS_TEST_MODE=1 below makes the beads SDK create testdb_<hash> databases.
// By routing those to an isolated container (via BEADS_DOLT_PORT), the
// databases are destroyed when the container is terminated at cleanup —
// preventing orphan accumulation in the shared production Dolt data dir.
//
// When Docker is unavailable, Dolt-needing tests self-skip via setupTestStore
// → beadsdk.Open failure. Non-Dolt tests (e.g. boot_spawn_frequency_test.go)
// still run. (fixes gt-kw4449)
func TestMain(m *testing.M) {
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

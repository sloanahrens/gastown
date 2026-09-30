package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// runDaemonTests is the body of both tiers' TestMain: it runs the package's
// tests under the hermetic harness (gt-lwi) — GT_*/BD_* env scrubbed, HOME
// and town root redirected to a sandbox, and a tripwire that fails the run if
// state leaks into a live town — and returns the exit code. setup, when not
// nil, runs once the harness has scrubbed the environment.
func runDaemonTests(m *testing.M, setup func(), opts ...testutil.HermeticOption) int {
	// Signal-target helper (pid_identity_test.go): this binary re-executed
	// under a chosen argv0/argv so a test owns a process that looks like
	// `gt daemon run` or `dolt sql-server`, and can prove the stop paths
	// signal it — without any test ever pointing a stop path at a host PID.
	if os.Getenv(signalTargetHelperEnv) == "1" {
		runSignalTargetHelper()
	}

	h, err := testutil.StartHermetic(opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon TestMain: %v\n", err)
		return 1
	}
	if setup != nil {
		setup()
	}

	// No per-package tmux socket: the harness already binds tmux's default to
	// an isolated per-process socket (and kills it at Finish), and the unit
	// tier drives a fakeTmux rather than a server.
	code := m.Run()

	return h.Finish(code)
}

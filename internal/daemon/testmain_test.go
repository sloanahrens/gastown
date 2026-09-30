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

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

	// Isolate tmux sessions on a package-specific socket.
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
	return h.Finish(code)
}

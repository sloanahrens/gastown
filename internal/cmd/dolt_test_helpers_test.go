//go:build integration

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/testutil"
)

// requireDoltServer delegates to testutil.RequireDoltContainer. The shared
// container serves only databases from its pool (bd init through
// beads.RunTestContainerInit or an isolated beads.Init); a test whose code
// under test names its own databases — gt install, gt rig add — uses
// requireScratchDoltServer instead.
func requireDoltServer(t *testing.T) {
	t.Helper()
	testutil.RequireDoltContainer(t)
}

// requireScratchDoltServer leases t the package's scratch Dolt container and
// points this process and its gt/bd subprocesses at it for the rest of the
// test.
//
// gt install and gt rig add create the databases they name (hq, the rig's),
// and a later test creates the same names again. On the shared container that
// is a catalog change while other tests run, which the pool's teardown guard
// fails the package for (internal/testutil/doltpool.go), and the next test
// meets the last one's databases. A lease holds one scratch container
// exclusively and drops it back to its starting catalog when the test ends, so
// these tests share a small pool instead of starting a container each
// (gt-16rk2, gt-6u1qd).
func requireScratchDoltServer(t *testing.T) {
	t.Helper()
	testutil.LeaseScratchDoltContainer(t)
}

// configureTestGitIdentity sets git global config in an isolated HOME directory
// so that EnsureDoltIdentity (called during gt install preflight) can copy
// identity from git to dolt.
func configureTestGitIdentity(t *testing.T, homeDir string) {
	t.Helper()
	env := append(os.Environ(), "HOME="+homeDir)
	for _, args := range [][]string{
		{"config", "--global", "user.name", "Test User"},
		{"config", "--global", "user.email", "test@test.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
}

// bridgeDoltPidToTown writes the Go test process PID into townRoot/daemon/dolt.pid
// so that doltserver.IsRunning(townRoot) finds it via the PID-file shortcut path.
//
// With containers there is no dolt binary PID file. We write our own process PID
// instead — the PID file's purpose is to make IsRunning() take the PID-file
// shortcut path (which just checks if the PID is alive, not the process name).
func bridgeDoltPidToTown(t *testing.T, townRoot string) {
	t.Helper()
	bridgeDoltPidToTownOnPort(t, townRoot, os.Getenv("GT_DOLT_PORT"))
}

// bridgeDoltPidToTownOnPort is bridgeDoltPidToTown for a test Dolt server on
// port, for a parallel test that cannot point GT_DOLT_PORT at it.
func bridgeDoltPidToTownOnPort(t *testing.T, townRoot, port string) {
	t.Helper()

	pid := fmt.Sprintf("%d", os.Getpid())

	daemonDir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatalf("bridgeDoltPidToTown: mkdir daemon: %v", err)
	}
	townPidPath := filepath.Join(daemonDir, "dolt.pid")
	if err := os.WriteFile(townPidPath, []byte(pid+"\n"), 0644); err != nil { //nolint:gosec
		t.Fatalf("bridgeDoltPidToTown: write PID file: %v", err)
	}

	// gt finds the server through the town's endpoint, never GT_DOLT_PORT
	// (gt-y3pgh.3): give a town without one the test server's port, the
	// listener gt dolt start would have written.
	if _, ok := config.ResolveDoltEndpoint(townRoot); ok {
		return
	}
	if port == "" {
		t.Fatal("bridgeDoltPidToTown: no test Dolt server port")
	}
	dataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatalf("bridgeDoltPidToTown: mkdir .dolt-data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("listener:\n  port: "+port+"\n"), 0600); err != nil {
		t.Fatalf("bridgeDoltPidToTown: write config.yaml: %v", err)
	}
}

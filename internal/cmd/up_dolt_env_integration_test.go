//go:build integration

package cmd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// gt up's Dolt environment and readiness wait against the real process
// environment and real sockets: applyConfiguredDoltEnv exists to rewrite
// os.Environ, and WaitForReady dials the configured port.

func TestIntegrationUpApplyConfiguredDoltEnvConfigBeatsStaleEnv(t *testing.T) {
	townRoot := t.TempDir()
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  host: 127.0.0.2\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_DOLT_HOST", "stale-host")
	t.Setenv("GT_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_PORT", "9999")

	applyConfiguredDoltEnv(townRoot)

	if got := os.Getenv("GT_DOLT_HOST"); got != "127.0.0.2" {
		t.Fatalf("GT_DOLT_HOST = %q, want 127.0.0.2", got)
	}
	if got := os.Getenv("GT_DOLT_PORT"); got != "5507" {
		t.Fatalf("GT_DOLT_PORT = %q, want 5507", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "127.0.0.2" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want 127.0.0.2", got)
	}
	if got := os.Getenv("BEADS_DOLT_PORT"); got != "5507" {
		t.Fatalf("BEADS_DOLT_PORT = %q, want 5507", got)
	}
}

func TestIntegrationUpApplyConfiguredDoltEnvClearsStaleHostWhenConfigHasNoHost(t *testing.T) {
	townRoot := t.TempDir()
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_DOLT_HOST", "stale-host")
	t.Setenv("GT_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")

	applyConfiguredDoltEnv(townRoot)

	if got := os.Getenv("GT_DOLT_HOST"); got != "" {
		t.Fatalf("GT_DOLT_HOST = %q, want cleared", got)
	}
	if got := os.Getenv("GT_DOLT_PORT"); got != "5507" {
		t.Fatalf("GT_DOLT_PORT = %q, want 5507", got)
	}
}

func TestIntegrationWaitForDoltReady_ServerListening(t *testing.T) {
	// When server is already listening, should return quickly.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port

	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","dolt_mode":"server","port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	writeTownDoltPort(t, townRoot, port)

	start := time.Now()
	waitForDoltReady(townRoot)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("waitForDoltReady took %v, should complete quickly when server is ready", elapsed)
	}
}

func TestIntegrationWaitForDoltReady_GracefulDegradation(t *testing.T) {
	// Verify that waitForDoltReady doesn't panic or error when Dolt is unreachable.
	// The wrapper should log a warning and continue (graceful degradation).
	// Uses a town root with no server metadata so it returns immediately.
	townRoot := t.TempDir()
	waitForDoltReady(townRoot) // Should not panic

	// Also verify the underlying doltserver.WaitForReady detects unreachable servers.
	// Use bind-then-close to find a guaranteed-free port. (review finding #4)
	tmpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	freePort := tmpListener.Addr().(*net.TCPAddr).Port
	tmpListener.Close()

	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","dolt_mode":"server","port":%d}`, freePort)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	writeTownDoltPort(t, townRoot, freePort)

	// WaitForReady with short timeout should fail when nothing is listening
	err = doltserver.WaitForReady(townRoot, 200*time.Millisecond)
	if err == nil {
		t.Error("doltserver.WaitForReady should fail when nothing is listening")
	}
}

func TestIntegrationWaitForDoltReady_WrapperTimesOutAndContinues(t *testing.T) {
	// Verify the wrapper's error handling path: WaitForReady returns an error
	// but the wrapper logs a warning and continues (doesn't panic or propagate).
	// We test WaitForReady directly with a short timeout to verify the error,
	// since the wrapper uses the package-level 10s constant. (review finding #5)
	tmpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := tmpListener.Addr().(*net.TCPAddr).Port
	tmpListener.Close()

	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","dolt_mode":"server","port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	writeTownDoltPort(t, townRoot, port)

	// Verify the error path fires when nothing listens.
	err = doltserver.WaitForReady(townRoot, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected WaitForReady to fail when nothing is listening")
	}
	// Confirm error message is actionable.
	if err.Error() == "" {
		t.Error("expected non-empty error message from WaitForReady timeout")
	}
}

// writeTownDoltPort gives the town at townRoot the Dolt endpoint port, the
// only place gt reads it from (gt-y3pgh.3).
func writeTownDoltPort(t *testing.T, townRoot string, port int) {
	t.Helper()
	dir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(fmt.Sprintf("listener:\n  port: %d\n", port)), 0644); err != nil {
		t.Fatal(err)
	}
}

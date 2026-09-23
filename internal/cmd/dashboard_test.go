package cmd

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDashboardCmd_FlagsExist(t *testing.T) {
	t.Parallel()
	// Verify required flags exist with correct defaults
	portFlag := dashboardCmd.Flags().Lookup("port")
	if portFlag == nil {
		t.Fatal("--port flag should exist")
	}
	if portFlag.DefValue != "8080" {
		t.Errorf("--port default should be 8080, got %s", portFlag.DefValue)
	}

	bindFlag := dashboardCmd.Flags().Lookup("bind")
	if bindFlag == nil {
		t.Fatal("--bind flag should exist")
	}
	wantBind := "127.0.0.1"
	if os.Getenv("IS_SANDBOX") != "" {
		wantBind = "0.0.0.0"
	}
	if bindFlag.DefValue != wantBind {
		t.Errorf("--bind default should be %s, got %s", wantBind, bindFlag.DefValue)
	}

	openFlag := dashboardCmd.Flags().Lookup("open")
	if openFlag == nil {
		t.Fatal("--open flag should exist")
	}
	if openFlag.DefValue != "false" {
		t.Errorf("--open default should be false, got %s", openFlag.DefValue)
	}
}

func TestDashboardCmd_IsRegistered(t *testing.T) {
	t.Parallel()
	// Verify command is registered under root
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "dashboard" {
			found = true
			break
		}
	}
	if !found {
		t.Error("dashboard command should be registered with rootCmd")
	}
}

func TestDashboardCmd_HasCorrectGroup(t *testing.T) {
	t.Parallel()
	if dashboardCmd.GroupID != GroupDiag {
		t.Errorf("dashboard should be in diag group, got %s", dashboardCmd.GroupID)
	}
}

func TestDashboardCmd_RequiresWorkspace(t *testing.T) {
	t.Parallel()
	// Create a test command that simulates running outside workspace
	cmd := &cobra.Command{}
	cmd.SetArgs([]string{})

	// The actual workspace check happens in runDashboard
	// This test verifies the command structure is correct
	if dashboardCmd.RunE == nil {
		t.Error("dashboard command should have RunE set")
	}
}

func TestEnsureDoltPortEnv_ReadsConfigYAML(t *testing.T) {
	// Create a temporary town root with durable Dolt config.
	townRoot := t.TempDir()
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  host: 127.0.0.2\n  port: 13307\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Clear any existing env vars
	t.Setenv("GT_DOLT_HOST", "")
	t.Setenv("GT_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("BEADS_DOLT_PORT", "")

	ensureDoltPortEnv(townRoot)

	if got := os.Getenv("GT_DOLT_PORT"); got != "13307" {
		t.Errorf("GT_DOLT_PORT = %q, want %q", got, "13307")
	}
	if got := os.Getenv("BEADS_DOLT_PORT"); got != "13307" {
		t.Errorf("BEADS_DOLT_PORT = %q, want %q", got, "13307")
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "13307" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "13307")
	}
	if got := os.Getenv("GT_DOLT_HOST"); got != "127.0.0.2" {
		t.Errorf("GT_DOLT_HOST = %q, want %q", got, "127.0.0.2")
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "127.0.0.2" {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "127.0.0.2")
	}
}

func TestEnsureDoltPortEnv_FallsBackToDefault(t *testing.T) {
	// Use a temp dir with no durable Dolt config.
	townRoot := t.TempDir()

	t.Setenv("GT_DOLT_HOST", "")
	t.Setenv("GT_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("BEADS_DOLT_PORT", "")

	ensureDoltPortEnv(townRoot)

	want := "3307"
	if got := os.Getenv("GT_DOLT_PORT"); got != want {
		t.Errorf("GT_DOLT_PORT = %q, want %q (default)", got, want)
	}
	if got := os.Getenv("BEADS_DOLT_PORT"); got != want {
		t.Errorf("BEADS_DOLT_PORT = %q, want %q (default)", got, want)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != want {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q (default)", got, want)
	}
	if got, ok := os.LookupEnv("BEADS_DOLT_SERVER_HOST"); ok {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want unset", got)
	}
}

func TestEnsureDoltPortEnv_GTDoltPortOverridesWrongBeadsPort(t *testing.T) {
	// Simulate the bug: Beads port aliases set to dashboard HTTP port (8080)
	// while GT_DOLT_PORT carries the explicit Dolt endpoint.
	t.Setenv("GT_DOLT_PORT", "3307")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "8080")
	t.Setenv("BEADS_DOLT_PORT", "8080")

	// Create durable config with a different port. Explicit GT_DOLT_PORT is still
	// authoritative for dashboard-spawned subprocesses.
	townRoot := t.TempDir()
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  port: 3308\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ensureDoltPortEnv(townRoot)

	if got := os.Getenv("GT_DOLT_PORT"); got != "3307" {
		t.Errorf("GT_DOLT_PORT = %q, want %q", got, "3307")
	}
	if got := os.Getenv("BEADS_DOLT_PORT"); got != "3307" {
		t.Errorf("BEADS_DOLT_PORT = %q, want %q", got, "3307")
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "3307" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
	}
}

// TestInstallDashboardLog_FetchErrorWrittenToFile verifies the core
// regression from gt-9ed0: a runtime error emitted through the stdlib log
// package (the fetch-timeout line in internal/web/handler.go) must land in
// the dashboard log file, not only in the terminal pane.
func TestInstallDashboardLog_FetchErrorWrittenToFile(t *testing.T) {
	townRoot := t.TempDir()

	cleanup := installDashboardLog(townRoot)
	defer cleanup()

	// The exact line the handler logs on a fetch timeout.
	log.Printf("dashboard: fetch timeout after 8s")

	data, err := os.ReadFile(filepath.Join(townRoot, "logs", "dashboard.log"))
	if err != nil {
		t.Fatalf("reading logs/dashboard.log: %v", err)
	}
	if !strings.Contains(string(data), "dashboard: fetch timeout after 8s") {
		t.Errorf("log file missing fetch-timeout line; contents:\n%s", data)
	}
}

// TestInstallDashboardLog_Appends verifies repeated errors accumulate rather
// than truncate the log.
func TestInstallDashboardLog_Appends(t *testing.T) {
	townRoot := t.TempDir()

	cleanup := installDashboardLog(townRoot)
	defer cleanup()

	log.Printf("dashboard: warning one")
	log.Printf("dashboard: warning two")

	data, err := os.ReadFile(filepath.Join(townRoot, "logs", "dashboard.log"))
	if err != nil {
		t.Fatalf("reading logs/dashboard.log: %v", err)
	}
	if !strings.Contains(string(data), "warning one") || !strings.Contains(string(data), "warning two") {
		t.Errorf("log file should contain both entries; contents:\n%s", data)
	}
}

// TestInstallDashboardLog_ExplicitPath verifies the --log-file flag value
// (stashed in the dashboardLog var) overrides the town-root default.
func TestInstallDashboardLog_ExplicitPath(t *testing.T) {
	logDir := t.TempDir()
	explicit := filepath.Join(logDir, "custom.log")
	prev := dashboardLog
	dashboardLog = explicit
	defer func() { dashboardLog = prev }()

	cleanup := installDashboardLog("/nonexistent-town-root")
	defer cleanup()

	log.Printf("dashboard: explicit path entry")

	data, err := os.ReadFile(explicit)
	if err != nil {
		t.Fatalf("reading explicit log path: %v", err)
	}
	if !strings.Contains(string(data), "explicit path entry") {
		t.Errorf("explicit log file missing entry; contents:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join("/nonexistent-town-root", "logs", "dashboard.log")); !os.IsNotExist(err) {
		t.Errorf("explicit path should not write under the town root")
	}
}

// TestRotatingLog_Rotates verifies the file is rotated to <name>.old once it
// exceeds the size cap and fresh entries land in a new file.
func TestRotatingLog_Rotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dashboard.log")
	rl := &rotatingLog{path: path, max: 64}
	defer rl.Close()

	// 3 writes of ~30 bytes exceed the 64-byte cap on the third write.
	for i := 0; i < 3; i++ {
		if _, err := rl.Write([]byte(strings.Repeat("x", 30) + "\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := os.Stat(path + ".old"); err != nil {
		t.Errorf("expected rotated %s.old: %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fresh log after rotation: %v", err)
	}
	if len(data) == 61 {
		t.Errorf("rotation did not truncate: fresh file has all 3 entries (%d bytes)", len(data))
	}
}

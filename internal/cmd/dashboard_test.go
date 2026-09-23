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

// dashLogEntry is one 31-byte line, the unit the rotation tests count.
var dashLogEntry = []byte(strings.Repeat("x", 30) + "\n")

// isolateDashboardLog clears the dashboardLog override for the duration of a
// test. dashboardLog takes its initial value from $GT_DASHBOARD_LOG, which a
// polecat or CI shell may export, and a test that leaves that value in place
// appends to the real log instead of its temp dir (gt-9ed0).
func isolateDashboardLog(t *testing.T) {
	t.Helper()
	prev := dashboardLog
	dashboardLog = ""
	t.Cleanup(func() { dashboardLog = prev })
}

// TestInstallDashboardLog_FetchErrorWrittenToFile verifies the core
// regression from gt-9ed0: a runtime error emitted through the stdlib log
// package (the fetch-timeout line in internal/web/handler.go) must land in
// the dashboard log file, not only in the terminal pane.
func TestInstallDashboardLog_FetchErrorWrittenToFile(t *testing.T) {
	isolateDashboardLog(t)
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
	isolateDashboardLog(t)
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

// TestInstallDashboardLog_IgnoresAmbientEnvOverride guards the isolation
// above: with dashboardLog pre-seeded from the environment, a test must still
// write only under its own temp dir.
func TestInstallDashboardLog_IgnoresAmbientEnvOverride(t *testing.T) {
	ambient := filepath.Join(t.TempDir(), "ambient.log")
	prev := dashboardLog
	dashboardLog = ambient
	t.Cleanup(func() { dashboardLog = prev })

	isolateDashboardLog(t)
	townRoot := t.TempDir()

	cleanup := installDashboardLog(townRoot)
	defer cleanup()

	log.Printf("dashboard: isolated entry")

	if _, err := os.Stat(ambient); !os.IsNotExist(err) {
		t.Errorf("ambient log %s was written despite isolation (stat err = %v)", ambient, err)
	}
	data, err := os.ReadFile(filepath.Join(townRoot, "logs", "dashboard.log"))
	if err != nil {
		t.Fatalf("reading logs/dashboard.log: %v", err)
	}
	if !strings.Contains(string(data), "isolated entry") {
		t.Errorf("log file missing entry; contents:\n%s", data)
	}
}

// TestInstallDashboardLog_ExplicitPath verifies the --log-file value (stashed
// in the dashboardLog var) wins over the town root, and is the only way setup
// mode writes a file at all.
func TestInstallDashboardLog_ExplicitPath(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "custom.log")
	prev := dashboardLog
	dashboardLog = explicit
	t.Cleanup(func() { dashboardLog = prev })

	// No town root: setup mode.
	cleanup := installDashboardLog("")
	defer cleanup()

	log.Printf("dashboard: explicit path entry")

	data, err := os.ReadFile(explicit)
	if err != nil {
		t.Fatalf("reading explicit log path: %v", err)
	}
	if !strings.Contains(string(data), "explicit path entry") {
		t.Errorf("explicit log file missing entry; contents:\n%s", data)
	}
}

// TestInstallDashboardLog_SetupModeCreatesNoDirectory verifies setup mode (no
// town root, no --log-file) leaves the working directory alone rather than
// creating a logs/ directory in it (gt-9ed0).
func TestInstallDashboardLog_SetupModeCreatesNoDirectory(t *testing.T) {
	isolateDashboardLog(t)
	dir := t.TempDir()
	t.Chdir(dir)

	cleanup := installDashboardLog("")
	defer cleanup()

	log.Printf("dashboard: setup mode entry")

	if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
		t.Errorf("setup mode created logs/ in the working directory (stat err = %v)", err)
	}
}

// TestRotatingLog_Rotates verifies the file is rotated to <name>.old once it
// exceeds the size cap, that .old holds the entries written before the
// rotation, and that the fresh file holds exactly the entry that triggered it.
func TestRotatingLog_Rotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dashboard.log")
	rl := &rotatingLog{path: path, max: 64}
	defer rl.Close()

	// 31 bytes per write: two fit under the 64-byte cap, the third crosses it.
	for i := 0; i < 3; i++ {
		if _, err := rl.Write(dashLogEntry); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	old, err := os.ReadFile(path + ".old")
	if err != nil {
		t.Fatalf("expected rotated %s.old: %v", path, err)
	}
	if got, want := len(old), 2*len(dashLogEntry); got != want {
		t.Errorf("rotated file holds %d bytes, want %d (the first two entries)", got, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fresh log after rotation: %v", err)
	}
	if got, want := len(data), len(dashLogEntry); got != want {
		t.Errorf("fresh log holds %d bytes, want %d (only the third entry)", got, want)
	}
}

// TestRotatingLog_RenameFailureKeepsAppending verifies that a rename that
// cannot succeed stops rotation without dropping entries or retrying the
// close/reopen dance on every later write.
func TestRotatingLog_RenameFailureKeepsAppending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dashboard.log")
	// A non-empty directory at <name>.old makes os.Rename fail, standing in
	// for a permission problem on the log directory.
	if err := os.MkdirAll(filepath.Join(path+".old", "child"), 0o755); err != nil {
		t.Fatalf("creating rename blocker: %v", err)
	}

	rl := &rotatingLog{path: path, max: 64}
	defer rl.Close()

	for i := 0; i < 4; i++ {
		if _, err := rl.Write(dashLogEntry); err != nil {
			t.Fatalf("write %d after failed rotation: %v", i, err)
		}
	}

	if !rl.rotateFailed {
		t.Error("rotateFailed = false after a rename failure, want true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log after failed rotation: %v", err)
	}
	if got, want := len(data), 4*len(dashLogEntry); got != want {
		t.Errorf("log holds %d bytes, want %d (every entry retained)", got, want)
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/wisp"
)

func TestDefaultConfig(t *testing.T) {
	t.Parallel()
	townRoot := "/tmp/test-town"
	config := DefaultConfig(townRoot)

	if config.HeartbeatInterval != 5*time.Minute {
		t.Errorf("expected HeartbeatInterval 5m, got %v", config.HeartbeatInterval)
	}
	if config.TownRoot != townRoot {
		t.Errorf("expected TownRoot %q, got %q", townRoot, config.TownRoot)
	}
	if config.LogFile != filepath.Join(townRoot, "daemon", "daemon.log") {
		t.Errorf("expected LogFile in daemon dir, got %q", config.LogFile)
	}
	if config.PidFile != filepath.Join(townRoot, "daemon", "daemon.pid") {
		t.Errorf("expected PidFile in daemon dir, got %q", config.PidFile)
	}
}

func TestDaemonPathCandidatesIncludesLaunchdToolDirs(t *testing.T) {
	t.Parallel()
	home := filepath.Join("Users", "alice")
	exePath := filepath.Join("opt", "homebrew", "bin", "gt")

	got := daemonPathCandidates(home, exePath)
	for _, want := range []string{
		filepath.Dir(exePath),
		filepath.Join(home, ".local/bin"),
		filepath.Join(home, "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("daemonPathCandidates(%q, %q) missing %q; got %v", home, exePath, want, got)
		}
	}
}

func TestCleanupLegacySocketSessionsRunsOnce(t *testing.T) {
	t.Parallel()
	var calls int
	var gotRoot string
	townRoot := t.TempDir()
	d := &Daemon{
		config: DefaultConfig(townRoot),
		logger: log.New(io.Discard, "", 0),
	}
	d.seams.cleanupLegacySockets = func(townRoot string) (int, int) {
		calls++
		gotRoot = townRoot
		return 1, 2
	}
	d.cleanupLegacySocketSessions()
	if calls != 1 {
		t.Fatalf("cleanup calls after first invocation = %d, want 1", calls)
	}
	if gotRoot != townRoot {
		t.Fatalf("cleanup townRoot = %q, want %q", gotRoot, townRoot)
	}

	d.cleanupLegacySocketSessions()
	if calls != 1 {
		t.Fatalf("cleanup calls after second invocation = %d, want 1", calls)
	}
}

func writeDaemonTownFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func TestStateFile(t *testing.T) {
	t.Parallel()
	townRoot := "/tmp/test-town"
	expected := filepath.Join(townRoot, "daemon", "state.json")
	result := StateFile(townRoot)

	if result != expected {
		t.Errorf("StateFile(%q) = %q, expected %q", townRoot, result, expected)
	}
}

func TestLoadState_NonExistent(t *testing.T) {
	t.Parallel()
	// Create temp dir that doesn't have a state file
	tmpDir, err := os.MkdirTemp("", "daemon-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	state, err := LoadState(tmpDir)
	if err != nil {
		t.Errorf("LoadState should not error for missing file, got %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil state")
	}
	if state.Running {
		t.Error("expected Running=false for empty state")
	}
	if state.PID != 0 {
		t.Errorf("expected PID=0 for empty state, got %d", state.PID)
	}
}

func TestLoadState_ExistingFile(t *testing.T) {
	t.Parallel()
	tmpDir, err := os.MkdirTemp("", "daemon-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create daemon directory
	daemonDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a state file
	startTime := time.Now().Truncate(time.Second)
	testState := &State{
		Running:        true,
		PID:            12345,
		StartedAt:      startTime,
		LastHeartbeat:  startTime,
		HeartbeatCount: 42,
	}

	data, err := json.MarshalIndent(testState, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(daemonDir, "state.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// Load and verify
	loaded, err := LoadState(tmpDir)
	if err != nil {
		t.Fatalf("LoadState error: %v", err)
	}
	if !loaded.Running {
		t.Error("expected Running=true")
	}
	if loaded.PID != 12345 {
		t.Errorf("expected PID=12345, got %d", loaded.PID)
	}
	if loaded.HeartbeatCount != 42 {
		t.Errorf("expected HeartbeatCount=42, got %d", loaded.HeartbeatCount)
	}
}

func TestLoadState_InvalidJSON(t *testing.T) {
	t.Parallel()
	tmpDir, err := os.MkdirTemp("", "daemon-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create daemon directory with invalid JSON
	daemonDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(daemonDir, "state.json"), []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err = LoadState(tmpDir)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestSaveState(t *testing.T) {
	t.Parallel()
	tmpDir, err := os.MkdirTemp("", "daemon-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	state := &State{
		Running:        true,
		PID:            9999,
		StartedAt:      time.Now(),
		LastHeartbeat:  time.Now(),
		HeartbeatCount: 100,
	}

	// SaveState should create daemon directory if needed
	if err := SaveState(tmpDir, state); err != nil {
		t.Fatalf("SaveState error: %v", err)
	}

	// Verify file exists
	stateFile := StateFile(tmpDir)
	if _, err := os.Stat(stateFile); err != nil {
		t.Errorf("state file should exist: %v", err)
	}

	// Verify contents
	loaded, err := LoadState(tmpDir)
	if err != nil {
		t.Fatalf("LoadState error: %v", err)
	}
	if loaded.PID != 9999 {
		t.Errorf("expected PID=9999, got %d", loaded.PID)
	}
	if loaded.HeartbeatCount != 100 {
		t.Errorf("expected HeartbeatCount=100, got %d", loaded.HeartbeatCount)
	}
}

func TestSaveLoadState_Roundtrip(t *testing.T) {
	t.Parallel()
	tmpDir, err := os.MkdirTemp("", "daemon-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	original := &State{
		Running:        true,
		PID:            54321,
		StartedAt:      time.Now().Truncate(time.Second),
		LastHeartbeat:  time.Now().Truncate(time.Second),
		HeartbeatCount: 1000,
	}

	if err := SaveState(tmpDir, original); err != nil {
		t.Fatalf("SaveState error: %v", err)
	}

	loaded, err := LoadState(tmpDir)
	if err != nil {
		t.Fatalf("LoadState error: %v", err)
	}

	if loaded.Running != original.Running {
		t.Errorf("Running mismatch: got %v, want %v", loaded.Running, original.Running)
	}
	if loaded.PID != original.PID {
		t.Errorf("PID mismatch: got %d, want %d", loaded.PID, original.PID)
	}
	if loaded.HeartbeatCount != original.HeartbeatCount {
		t.Errorf("HeartbeatCount mismatch: got %d, want %d", loaded.HeartbeatCount, original.HeartbeatCount)
	}
	// Time comparison with truncation to handle JSON serialization
	if !loaded.StartedAt.Truncate(time.Second).Equal(original.StartedAt) {
		t.Errorf("StartedAt mismatch: got %v, want %v", loaded.StartedAt, original.StartedAt)
	}
}

func TestListPolecatWorktrees_SkipsHiddenDirs(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	polecatsDir := filepath.Join(tmpDir, "some-rig", "polecats")

	if err := os.MkdirAll(filepath.Join(polecatsDir, ".claude"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(polecatsDir, "furiosa"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(polecatsDir, "not-a-dir.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	polecats, err := listPolecatWorktrees(polecatsDir)
	if err != nil {
		t.Fatalf("listPolecatWorktrees returned error: %v", err)
	}

	if slices.Contains(polecats, ".claude") {
		t.Fatalf("expected hidden dir .claude to be ignored, got %v", polecats)
	}
	if !slices.Contains(polecats, "furiosa") {
		t.Fatalf("expected furiosa to be included, got %v", polecats)
	}
}

// NOTE: TestIsWitnessSession removed - isWitnessSession function was deleted
// as part of ZFC cleanup. Witness poking is now Deacon's responsibility.

func TestIsShutdownInProgress_NoLockFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	d := &Daemon{
		config: &Config{TownRoot: tmpDir},
	}

	// No lock file exists - should return false
	if d.isShutdownInProgress() {
		t.Error("expected false when lock file doesn't exist")
	}
}

func TestIsShutdownInProgress_StaleLockFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, "shutdown.lock")

	// Create a stale lock file (not actually locked)
	if err := os.WriteFile(lockPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{
		config: &Config{TownRoot: tmpDir},
	}

	// File exists but not locked - should return false
	if d.isShutdownInProgress() {
		t.Error("expected false when lock file exists but is not locked")
	}

	// File should still exist - flock files are never removed to prevent
	// a race where concurrent callers lock different inodes
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		t.Error("expected lock file to be preserved (flock files should not be removed)")
	}
}

func TestIsShutdownInProgress_ActiveLock(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, "shutdown.lock")

	// Create and hold the lock (simulating active shutdown)
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		t.Fatalf("failed to acquire lock: %v", err)
	}
	if !locked {
		t.Fatal("expected to acquire lock")
	}
	defer func() { _ = lock.Unlock() }()

	d := &Daemon{
		config: &Config{TownRoot: tmpDir},
	}

	// File exists and is locked - should return true
	if !d.isShutdownInProgress() {
		t.Error("expected true when lock file is actively held")
	}

	// File should still exist (we're still holding the lock)
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lock file should still exist: %v", err)
	}
}

// TestIsRunningFromPID_StalePIDReturnsNoError verifies that isRunningFromPID
// returns (false, 0, nil) — not an error — when it finds and removes a stale
// PID file. This is the fix for GH#2107: `gt daemon start` was treating the
// stale cleanup as an error, showing help text instead of starting the daemon.
func TestIsRunningFromPID_StalePIDReturnsNoError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	daemonDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a PID file pointing to a process that doesn't exist.
	// PID 2^22-1 (4194303) is extremely unlikely to be in use.
	stalePID := 4194303
	pidFile := filepath.Join(daemonDir, "daemon.pid")
	if _, err := writePIDFile(pidFile, stalePID); err != nil {
		t.Fatal(err)
	}

	running, pid, err := isRunningFromPID(tmpDir)
	if err != nil {
		t.Errorf("isRunningFromPID should not return error for stale PID, got: %v", err)
	}
	if running {
		t.Error("expected running=false for stale PID")
	}
	if pid != 0 {
		t.Errorf("expected pid=0, got %d", pid)
	}

	// PID file should have been removed
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("expected stale PID file to be removed")
	}
}

// TestIsRunningFromPID_NoPIDFile verifies clean return when no PID file exists.
func TestIsRunningFromPID_NoPIDFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	daemonDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatal(err)
	}

	running, pid, err := isRunningFromPID(tmpDir)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if running {
		t.Error("expected running=false")
	}
	if pid != 0 {
		t.Errorf("expected pid=0, got %d", pid)
	}
}

// TestIsRunningFromPID_LiveProcess verifies detection of a live process.
func TestIsRunningFromPID_LiveProcess(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	daemonDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Use our own PID — guaranteed alive
	pidFile := filepath.Join(daemonDir, "daemon.pid")
	if _, err := writePIDFile(pidFile, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	running, pid, err := isRunningFromPID(tmpDir)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !running {
		t.Error("expected running=true for live process")
	}
	if pid != os.Getpid() {
		t.Errorf("expected pid=%d, got %d", os.Getpid(), pid)
	}
}

// TestIsRigOperational_FailSafeOnDoltUnavailable verifies that when Dolt is
// unavailable and we can't check the rig bead for docked status, we fail-safe
// by assuming the rig is NOT operational. This prevents wasting API credits
// starting witnesses for potentially docked rigs. (Regression test for
// bug where witnesses started for docked rigs during Dolt outage)
func TestIsRigOperational_FailSafeOnDoltUnavailable(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create a minimal rig structure without a beads database
	rigName := "testrig"
	rigPath := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Create config.json with a prefix
	configPath := filepath.Join(rigPath, "config.json")
	configJSON := `{"beads": {"prefix": "tr"}}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// Create mayor/rig/.beads directory but NO Dolt database
	// This simulates Dolt being down or database not accessible
	mayorBeads := filepath.Join(rigPath, "mayor", "rig", ".beads")
	if err := os.MkdirAll(mayorBeads, 0755); err != nil {
		t.Fatal(err)
	}

	// Create town-level .beads with routes.jsonl
	townBeads := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"tr-","path":"testrig/mayor/rig"}`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	registerTestRigs(t, tmpDir, map[string]string{rigName: "tr"})

	// A Dolt server that is down: the rig bead read fails.
	var asked []string
	d := &Daemon{
		config: &Config{
			TownRoot: tmpDir,
		},
		logger: log.New(io.Discard, "", 0), // Suppress log output
		rigBeadShowFn: func(_, id string) (*beads.Issue, error) {
			asked = append(asked, id)
			return nil, errors.New("dial tcp 127.0.0.1:3307: connect: connection refused")
		},
	}

	// When Dolt is unavailable, isRigOperational should return false
	// (fail-safe: assume not operational rather than risk starting docked rig)
	operational, reason := d.isRigOperational(rigName)
	if operational {
		t.Error("isRigOperational should return false when Dolt is unavailable (fail-safe)")
	}
	if reason == "" {
		t.Error("isRigOperational should provide a reason when returning false")
	}
	if !strings.Contains(reason, "Dolt unavailable") && !strings.Contains(reason, "cannot verify") {
		t.Errorf("reason should mention Dolt unavailable, got: %q", reason)
	}
	if len(asked) != 1 || asked[0] != "tr-rig-testrig" {
		t.Errorf("rig bead reads = %v, want the one read of tr-rig-testrig (prefix from config.json)", asked)
	}
}

// A rig with no config.json whose registry (rigs.json) exists but does not
// decode has no known prefix: queryRigBead must report that instead of reading
// a rig bead id built from the gt fallback, which belongs to another rig
// (gt-71dgi).
func TestQueryRigBead_CorruptRigsJSONReadsNoBead(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "testrig"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "mayor", "rigs.json"), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}

	var asked []string
	d := &Daemon{
		config: &Config{TownRoot: tmpDir},
		logger: log.New(io.Discard, "", 0),
		rigBeadShowFn: func(_, id string) (*beads.Issue, error) {
			asked = append(asked, id)
			return &beads.Issue{ID: id}, nil
		},
	}

	id, _, err := d.queryRigBead("testrig")
	if err == nil {
		t.Fatalf("queryRigBead with a corrupt rigs.json = (%q, nil error), want an error", id)
	}
	if !strings.Contains(err.Error(), "no beads prefix") {
		t.Errorf("error = %q, want it to say the rig has no beads prefix", err)
	}
	if len(asked) != 0 {
		t.Errorf("rig bead reads = %v, want none", asked)
	}
}

// TestIsRigOperational_DockedRig verifies that docked rigs are correctly
// identified as not operational.
func TestIsRigOperational_DockedRig(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create rig with docked label on rig bead
	rigName := "dockedrig"
	rigPath := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, "mayor", "rig", ".beads"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create config.json
	configPath := filepath.Join(rigPath, "config.json")
	configJSON := `{"beads": {"prefix": "dr"}}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// Create town-level .beads with routes.jsonl
	townBeads := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"dr-","path":"dockedrig/mayor/rig"}`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	registerTestRigs(t, tmpDir, map[string]string{rigName: "dr"})

	// Without a rig bead, should fail-safe to not operational.
	d := &Daemon{
		config: &Config{
			TownRoot: tmpDir,
		},
		logger: log.New(io.Discard, "", 0),
		rigBeadShowFn: func(_, id string) (*beads.Issue, error) {
			return nil, fmt.Errorf("no issue found matching %q", id)
		},
	}
	if operational, _ := d.isRigOperational(rigName); operational {
		t.Error("isRigOperational should return false when rig bead is missing")
	}

	// A rig bead labeled status:docked is not operational, and says why.
	d = &Daemon{
		config: &Config{
			TownRoot: tmpDir,
		},
		logger: log.New(io.Discard, "", 0),
		rigBeadShowFn: func(_, id string) (*beads.Issue, error) {
			if id != "dr-rig-dockedrig" {
				return nil, fmt.Errorf("no issue found matching %q", id)
			}
			return &beads.Issue{ID: id, Status: "open", Labels: []string{"gt:rig", "status:docked"}}, nil
		},
	}
	operational, reason := d.isRigOperational(rigName)
	if operational || reason != "rig is docked (global)" {
		t.Errorf("isRigOperational(docked rig) = %v, %q; want false, \"rig is docked (global)\"", operational, reason)
	}
}

// TestAutoRestartDisabled covers the auto_restart check in isRigOperational.
// The values are written the way the wisp layer stores them, including the
// numeric encodings that survive a JSON round-trip (float64), because a bare
// `val.(bool)` assertion ignores every one of those and silently leaves
// auto-restart enabled - which is what `gt rig config set <rig> auto_restart 0`
// did for a key whose value was stored as a number.
func TestAutoRestartDisabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		rawJSON  string // value as written into .beads-wisp/config/<rig>.json
		disabled bool
	}{
		{"unset stays enabled", "", false},
		{"true stays enabled", `true`, false},
		{"bare 1 stays enabled", `1`, false},
		{"false disables", `false`, true},
		{"bare 0 disables", `0`, true},
		{`"false" disables`, `"false"`, true},
		{`"0" disables`, `"0"`, true},
		{`"true" stays enabled`, `"true"`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			rigName := "testrig"

			values := map[string]interface{}{}
			if tc.rawJSON != "" {
				var raw interface{}
				if err := json.Unmarshal([]byte(tc.rawJSON), &raw); err != nil {
					t.Fatalf("bad test fixture %q: %v", tc.rawJSON, err)
				}
				values["auto_restart"] = raw
			}

			wispDir := filepath.Join(townRoot, wisp.WispConfigDir, wisp.ConfigSubdir)
			if err := os.MkdirAll(wispDir, 0755); err != nil {
				t.Fatal(err)
			}
			contents, err := json.Marshal(map[string]interface{}{
				"rig":     rigName,
				"values":  values,
				"blocked": []string{},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wispDir, rigName+".json"), contents, 0644); err != nil {
				t.Fatal(err)
			}

			cfg := wisp.NewConfig(townRoot, rigName)
			if got := autoRestartDisabled(cfg.Get("auto_restart")); got != tc.disabled {
				t.Errorf("autoRestartDisabled with auto_restart=%s = %v, want %v",
					tc.rawJSON, got, tc.disabled)
			}
		})
	}
}

// ownerEnv is the env seam a start runs under in these tests: a map stands in
// for the process environment, so a fixture sets one variable and the real
// environment never does.
func ownerEnv(kv map[string]string) func(string) string {
	return func(key string) string { return kv[key] }
}

// TestDetectSelf pins the owner a daemon names for itself from the environment
// that started it (gt-qpeei.12), in the order the design ranks them.
func TestDetectSelf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want Owner
	}{
		{
			name: "the town's container",
			env:  map[string]string{"GT_DAEMON_SUPERVISOR": "container", "GT_DAEMON_CONTAINER_NAME": "scratch-town-daemon-1"},
			want: Owner{Kind: "container", ID: "scratch-town-daemon-1"},
		},
		{
			name: "a launchd job",
			env:  map[string]string{"XPC_SERVICE_NAME": "com.gastown.daemon"},
			want: Owner{Kind: "launchd", ID: "com.gastown.daemon"},
		},
		{
			name: "a systemd unit",
			env:  map[string]string{"INVOCATION_ID": "9f2c4d1e"},
			want: Owner{Kind: "systemd", ID: "9f2c4d1e"},
		},
		{
			name: "a hand-run daemon",
			want: Owner{Kind: "manual", ID: fmt.Sprintf("fixture-host:%d", os.Getpid())},
		},
		{
			// macOS sets XPC_SERVICE_NAME to "0" in a plain user session, so
			// a daemon run by hand there is manual, not the launchd job "0".
			name: "a hand-run daemon in a macOS session",
			env:  map[string]string{"XPC_SERVICE_NAME": "0"},
			want: Owner{Kind: "manual", ID: fmt.Sprintf("fixture-host:%d", os.Getpid())},
		},
		{
			name: "a macOS session value of 0 does not outrank systemd",
			env:  map[string]string{"XPC_SERVICE_NAME": "0", "INVOCATION_ID": "9f2c4d1e"},
			want: Owner{Kind: "systemd", ID: "9f2c4d1e"},
		},
		{
			name: "container mode outranks the panel variables",
			env: map[string]string{
				"GT_DAEMON_SUPERVISOR":     "container",
				"GT_DAEMON_CONTAINER_NAME": "scratch-town-daemon-1",
				"XPC_SERVICE_NAME":         "com.gastown.daemon",
				"INVOCATION_ID":            "9f2c4d1e",
			},
			want: Owner{Kind: "container", ID: "scratch-town-daemon-1"},
		},
		{
			name: "launchd outranks systemd",
			env:  map[string]string{"XPC_SERVICE_NAME": "com.gastown.daemon", "INVOCATION_ID": "9f2c4d1e"},
			want: Owner{Kind: "launchd", ID: "com.gastown.daemon"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DetectSelf(ownerEnv(tc.env), fixtureHostname("fixture-host"))
			if err != nil {
				t.Fatalf("DetectSelf = %v, want %+v", err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("DetectSelf = %+v, want %+v", got, tc.want)
			}
		})
	}

	// Only the manual owner's id carries the host name, so only that path may
	// read it: a launchd, systemd or container start that failed here would be
	// refused for an id it never writes (gt-qpeei.12).
	t.Run("the service managers never read the hostname", func(t *testing.T) {
		t.Parallel()
		unreadable := func() (string, error) {
			t.Errorf("DetectSelf read the host name for an owner kind whose id does not carry it")
			return "", errors.New("no hostname")
		}
		for _, tc := range []struct {
			name string
			env  map[string]string
			want Owner
		}{
			{
				name: "launchd",
				env:  map[string]string{"XPC_SERVICE_NAME": "com.gastown.daemon"},
				want: Owner{Kind: "launchd", ID: "com.gastown.daemon"},
			},
			{
				name: "systemd",
				env:  map[string]string{"INVOCATION_ID": "9f2c4d1e"},
				want: Owner{Kind: "systemd", ID: "9f2c4d1e"},
			},
			{
				name: "container",
				env:  map[string]string{"GT_DAEMON_SUPERVISOR": "container", "GT_DAEMON_CONTAINER_NAME": "scratch-town-daemon-1"},
				want: Owner{Kind: "container", ID: "scratch-town-daemon-1"},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got, err := DetectSelf(ownerEnv(tc.env), unreadable)
				if err != nil {
					t.Fatalf("DetectSelf = %v, want %+v: this owner kind names itself without the host", err, tc.want)
				}
				if got != tc.want {
					t.Fatalf("DetectSelf = %+v, want %+v", got, tc.want)
				}
			})
		}
	})

	t.Run("container mode without a container name", func(t *testing.T) {
		t.Parallel()
		got, err := DetectSelf(ownerEnv(map[string]string{"GT_DAEMON_SUPERVISOR": "container"}), fixtureHostname("fixture-host"))
		if err == nil || !strings.Contains(err.Error(), "GT_DAEMON_CONTAINER_NAME") {
			t.Fatalf("DetectSelf = %+v, %v; want an error naming GT_DAEMON_CONTAINER_NAME, never a guessed id", got, err)
		}
	})
}

// fixtureHostname is the host a test's manual owner names, so no test reads
// the host it runs on.
func fixtureHostname(host string) func() (string, error) {
	return func() (string, error) { return host, nil }
}

// startParkedRun starts a Run on an empty temp town and parks it at the
// legacy-socket step, the first point after the start-time writes, so a test
// reads what a start leaves while the daemon is still up. finish ends the run
// and returns Run's error.
func startParkedRun(t *testing.T, env map[string]string) (*Config, func() error) {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(town)
	d := &Daemon{
		config:   cfg,
		logger:   log.New(io.Discard, "", 0),
		getenv:   ownerEnv(env),
		hostname: fixtureHostname("fixture-host"),
		clock:    clockwork.NewFakeClockAt(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)),
	}
	// The rest of the startup does real host work: the heartbeat dispatches
	// and restarts sessions, the sweep touches the tmux socket directory, and
	// the probes reach the network.
	d.disabledPatrols = map[string]bool{
		"landing_infra":    true,
		"resource_sampler": true,
		"spec_dispatch":    true,
	}
	d.seams.heartbeatWork = func(*Daemon, *State, bool) {}

	parked := make(chan struct{})
	release := make(chan struct{})
	d.seams.cleanupLegacySockets = func(string) (int, int) {
		close(parked)
		<-release
		return 0, 0
	}

	d.ctx, d.cancel = context.WithCancel(context.Background())
	t.Cleanup(d.cancel)
	done := make(chan error, 1)
	go func() { done <- d.Run() }()
	waitForSignal(t, parked, "Run never reached the end of its start-time writes")

	var (
		once      sync.Once
		runResult error
	)
	finish := func() error {
		once.Do(func() {
			d.cancel()
			close(release)
			bounded, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			select {
			case runResult = <-done:
			case <-bounded.Done():
				t.Fatal("Run did not return after the daemon context ended")
			}
		})
		return runResult
	}
	t.Cleanup(func() { _ = finish() })
	return cfg, finish
}

// TestRunWritesLaunchdOwner pins the owner at the call site that gives it
// meaning: a daemon launchd started records the job label as its id, at start
// and again at the shutdown save (gt-qpeei.12).
func TestRunWritesLaunchdOwner(t *testing.T) {
	t.Parallel()
	cfg, finish := startParkedRun(t, map[string]string{"XPC_SERVICE_NAME": "com.gastown.daemon"})

	state, err := LoadState(cfg.TownRoot)
	if err != nil {
		t.Fatalf("LoadState after the start-time writes = %v", err)
	}
	if want := (Owner{Kind: "launchd", ID: "com.gastown.daemon"}); state.Owner == nil || *state.Owner != want {
		t.Fatalf("state.json owner = %+v, want %+v", state.Owner, want)
	}

	if err := finish(); err != nil {
		t.Fatalf("Run after the daemon context ended = %v, want a clean shutdown", err)
	}
	stopped, err := LoadState(cfg.TownRoot)
	if err != nil {
		t.Fatalf("LoadState after the shutdown save = %v", err)
	}
	if stopped.Running {
		t.Errorf("state.json running = true after Run returned, want the shutdown's clean stop")
	}
	if want := (Owner{Kind: "launchd", ID: "com.gastown.daemon"}); stopped.Owner == nil || *stopped.Owner != want {
		t.Errorf("state.json owner after the shutdown save = %+v, want %+v: the clean stop must name who left it", stopped.Owner, want)
	}
}

// The owner reaches the writers that save a state they did not build: the
// heartbeat ticks the state it was handed. Naming the owner only in
// startState would leave them free to drop it (gt-qpeei.12).
func TestHeartbeatSaveKeepsTheOwner(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig(t.TempDir())
	want := Owner{Kind: "launchd", ID: "com.gastown.daemon"}
	d := &Daemon{
		config: cfg,
		logger: log.New(io.Discard, "", 0),
		owner:  &want,
	}
	d.seams.heartbeatStep = func(*Daemon, heartbeatStep) {}

	d.heartbeatWork(&State{Running: true}, false)

	state, err := LoadState(cfg.TownRoot)
	if err != nil {
		t.Fatalf("LoadState after the heartbeat save = %v", err)
	}
	if state.Owner == nil || *state.Owner != want {
		t.Fatalf("state.json owner after the heartbeat save = %+v, want %+v", state.Owner, want)
	}
}

// TestRunInContainerModeWritesNoPidFile pins container mode at Run: no pid
// file, because a pid from the container's namespace means nothing on the
// host, and an owner naming the container (gt-qpeei.12).
func TestRunInContainerModeWritesNoPidFile(t *testing.T) {
	t.Parallel()
	cfg, _ := startParkedRun(t, map[string]string{
		"GT_DAEMON_SUPERVISOR":     "container",
		"GT_DAEMON_CONTAINER_NAME": "scratch-town-daemon-1",
	})

	if _, err := os.Stat(cfg.PidFile); !os.IsNotExist(err) {
		t.Fatalf("Run wrote %s in container mode; a pid from another namespace means nothing on the host", cfg.PidFile)
	}
	state, err := LoadState(cfg.TownRoot)
	if err != nil {
		t.Fatalf("LoadState after the start-time writes = %v", err)
	}
	if want := (Owner{Kind: "container", ID: "scratch-town-daemon-1"}); state.Owner == nil || *state.Owner != want {
		t.Fatalf("state.json owner = %+v, want %+v", state.Owner, want)
	}
}

// TestRunOutsideContainerModeWritesPidFile keeps the host path as it was: a
// daemon no compose file started still writes the pid file its readers use.
func TestRunOutsideContainerModeWritesPidFile(t *testing.T) {
	t.Parallel()
	cfg, _ := startParkedRun(t, nil)

	if _, err := os.Stat(cfg.PidFile); err != nil {
		t.Fatalf("Run wrote no %s outside container mode: %v", cfg.PidFile, err)
	}
}

// A container-mode start that cannot name its container never touches the
// town: no lock, no pid file, no state, and the error names the variable the
// compose file failed to set (gt-qpeei.12).
func TestRunRefusesContainerModeWithoutAContainerName(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(town)
	d := &Daemon{
		config: cfg,
		logger: log.New(io.Discard, "", 0),
		getenv: ownerEnv(map[string]string{"GT_DAEMON_SUPERVISOR": "container"}),
	}
	// A start that got past the owner check would run the whole loop; a
	// canceled context ends it at the first select instead of parking the test.
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.cancel()

	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "GT_DAEMON_CONTAINER_NAME") {
		t.Fatalf("Run = %v, want a refusal naming GT_DAEMON_CONTAINER_NAME", err)
	}
	if _, err := os.Stat(cfg.PidFile); !os.IsNotExist(err) {
		t.Errorf("Run wrote %s before refusing", cfg.PidFile)
	}
	if _, err := os.Stat(StateFile(cfg.TownRoot)); !os.IsNotExist(err) {
		t.Errorf("Run wrote state.json before refusing")
	}
}

// A start that cannot read its hostname cannot name a manual owner, so it
// refuses before touching the town rather than record a guessed id
// (gt-qpeei.12).
func TestRunRefusesWhenItCannotNameItsHost(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(town)
	d := &Daemon{
		config:   cfg,
		logger:   log.New(io.Discard, "", 0),
		getenv:   ownerEnv(nil),
		hostname: func() (string, error) { return "", errors.New("no hostname") },
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.cancel()

	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "naming the daemon's owner") {
		t.Fatalf("Run = %v, want a refusal naming the owner", err)
	}
	if _, err := os.Stat(cfg.PidFile); !os.IsNotExist(err) {
		t.Errorf("Run wrote %s before refusing", cfg.PidFile)
	}
	if _, err := os.Stat(StateFile(cfg.TownRoot)); !os.IsNotExist(err) {
		t.Errorf("Run wrote state.json before refusing")
	}
}

// A pid file the daemon cannot write fails the start: a daemon no host reader
// can find by pid would take the town without leaving the one mark it has
// (gt-qpeei.12).
func TestRunRefusesWhenThePidFileCannotBeWritten(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(town)
	cfg.PidFile = filepath.Join(town, "daemon", "absent-dir", "daemon.pid")
	d := &Daemon{
		config:   cfg,
		logger:   log.New(io.Discard, "", 0),
		getenv:   ownerEnv(nil),
		hostname: fixtureHostname("fixture-host"),
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.cancel()

	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "writing PID file") {
		t.Fatalf("Run = %v, want a refusal naming the PID file", err)
	}
	if _, err := os.Stat(StateFile(cfg.TownRoot)); !os.IsNotExist(err) {
		t.Errorf("Run wrote state.json after the PID file failed")
	}
}

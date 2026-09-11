package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
)

// writeLivenessState writes daemon/state.json with the given heartbeat
// fields, creating the daemon directory as needed.
func writeLivenessState(t *testing.T, townRoot string, lastHeartbeat time.Time, count int64) {
	t.Helper()
	state := &daemon.State{
		Running:        true,
		LastHeartbeat:  lastHeartbeat,
		HeartbeatCount: count,
	}
	if err := daemon.SaveState(townRoot, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
}

// writeLivenessBaseline writes the check's own baseline file with a
// controlled RecordedAt, bypassing Run so tests can simulate a baseline
// recorded by "a previous run at least staleThreshold earlier".
func writeLivenessBaseline(t *testing.T, townRoot string, count int64, recordedAt time.Time) {
	t.Helper()
	path := daemonLivenessStatePath(townRoot)
	if err := writeDaemonLivenessBaseline(path, daemonLivenessBaseline{HeartbeatCount: count, RecordedAt: recordedAt}); err != nil {
		t.Fatalf("writeDaemonLivenessBaseline: %v", err)
	}
}

// writeRecoveryHeartbeatInterval writes an explicit recovery_heartbeat_interval
// into the town's settings/config.json, so tests pin staleThreshold (2x this
// value) to a known value instead of relying on the compiled-in default.
func writeRecoveryHeartbeatInterval(t *testing.T, townRoot string, interval string) {
	t.Helper()
	settings := &config.TownSettings{
		Operational: &config.OperationalConfig{
			Daemon: &config.DaemonThresholds{RecoveryHeartbeatInterval: interval},
		},
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), settings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
}

func TestDaemonLivenessCheck_FreshAdvancing_OK(t *testing.T) {
	townRoot := t.TempDir()
	writeRecoveryHeartbeatInterval(t, townRoot, "3m") // staleThreshold = 6m
	writeLivenessBaseline(t, townRoot, 5, time.Now().Add(-10*time.Minute))
	writeLivenessState(t, townRoot, time.Now().Add(-1*time.Minute), 6)

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
	}
}

func TestDaemonLivenessCheck_FreshStatic_Error(t *testing.T) {
	townRoot := t.TempDir()
	writeRecoveryHeartbeatInterval(t, townRoot, "3m") // staleThreshold = 6m
	writeLivenessBaseline(t, townRoot, 5, time.Now().Add(-10*time.Minute))
	writeLivenessState(t, townRoot, time.Now().Add(-1*time.Minute), 5)

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError; message: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "count not advancing") {
		t.Errorf("Message = %q, want it to name %q", result.Message, "count not advancing")
	}

	// Once flagged, the baseline must refresh so the daemon isn't re-flagged
	// forever once it recovers.
	baseline, ok := readDaemonLivenessBaseline(daemonLivenessStatePath(townRoot))
	if !ok {
		t.Fatal("expected the baseline to still be present")
	}
	if !baseline.RecordedAt.After(time.Now().Add(-1 * time.Minute)) {
		t.Errorf("baseline RecordedAt = %v, want refreshed to ~now after a stuck-count verdict", baseline.RecordedAt)
	}
}

func TestDaemonLivenessCheck_FreshStatic_BaselineTooYoung_OK(t *testing.T) {
	// The baseline exists but hasn't had a fair chance to advance yet
	// (< staleThreshold old) — must not be flagged as stuck.
	townRoot := t.TempDir()
	writeRecoveryHeartbeatInterval(t, townRoot, "3m") // staleThreshold = 6m
	writeLivenessBaseline(t, townRoot, 5, time.Now().Add(-1*time.Minute))
	writeLivenessState(t, townRoot, time.Now().Add(-1*time.Minute), 5)

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
	}
}

func TestDaemonLivenessCheck_FrozenCount_BaselineNotResetWhileYoung(t *testing.T) {
	// Regression: the baseline used to be rewritten to "now" on every run
	// regardless of verdict, so a doctor cadence shorter than staleThreshold
	// kept baselineAge under the threshold forever and "count not advancing"
	// could never fire — the check silently degraded to timestamp-only
	// freshness. A young baseline with a frozen count must be left alone so
	// its age keeps growing across runs.
	townRoot := t.TempDir()
	writeRecoveryHeartbeatInterval(t, townRoot, "1m") // staleThreshold = 2m
	statePath := daemonLivenessStatePath(townRoot)

	originalRecordedAt := time.Now().Add(-30 * time.Second)
	writeLivenessBaseline(t, townRoot, 5, originalRecordedAt)
	writeLivenessState(t, townRoot, time.Now(), 5)

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
	}

	baseline, ok := readDaemonLivenessBaseline(statePath)
	if !ok {
		t.Fatal("expected the baseline to still be present")
	}
	if !baseline.RecordedAt.Equal(originalRecordedAt) {
		t.Errorf("baseline RecordedAt = %v, want unchanged %v (a stuck count's detection window must not reset on every run)", baseline.RecordedAt, originalRecordedAt)
	}
}

func TestDaemonLivenessCheck_Stale_Error(t *testing.T) {
	townRoot := t.TempDir()
	writeRecoveryHeartbeatInterval(t, townRoot, "3m") // staleThreshold = 6m
	writeLivenessBaseline(t, townRoot, 5, time.Now().Add(-20*time.Minute))
	writeLivenessState(t, townRoot, time.Now().Add(-10*time.Minute), 5)

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError; message: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "stale") {
		t.Errorf("Message = %q, want it to name %q", result.Message, "stale")
	}
}

func TestDaemonLivenessCheck_NotRunning_WarningNotError(t *testing.T) {
	// A cleanly stopped daemon leaves LastHeartbeat/HeartbeatCount intact
	// (daemon.go's stop path only flips Running to false), so a stale
	// heartbeat here proves nothing. Must warn, not error — matching
	// DaemonCheck's StatusWarning for "not running" so doctor's exit code
	// stays consistent between the two checks.
	townRoot := t.TempDir()
	state := &daemon.State{
		Running:        false,
		LastHeartbeat:  time.Now().Add(-1 * time.Hour),
		HeartbeatCount: 5,
	}
	if err := daemon.SaveState(townRoot, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "not running") {
		t.Errorf("Message = %q, want it to name %q", result.Message, "not running")
	}

	report := NewReport()
	report.Add(result)
	if report.HasErrors() {
		t.Error("a stopped daemon must not count as a doctor error (exit code parity with DaemonCheck)")
	}
}

func TestDaemonLivenessCheck_Unreadable_Skipped(t *testing.T) {
	townRoot := t.TempDir()
	stateFile := daemon.StateFile(townRoot)
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusSkipped {
		t.Errorf("Status = %v, want StatusSkipped; message: %s", result.Status, result.Message)
	}
}

func TestDaemonLivenessCheck_MissingState_Skipped(t *testing.T) {
	townRoot := t.TempDir()

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusSkipped {
		t.Errorf("Status = %v, want StatusSkipped; message: %s", result.Status, result.Message)
	}
}

func TestDaemonLivenessCheck_NoBaseline_SkippedAndRecordsOne(t *testing.T) {
	townRoot := t.TempDir()
	writeLivenessState(t, townRoot, time.Now().Add(-1*time.Minute), 3)

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped; message: %s", result.Status, result.Message)
	}

	baseline, ok := readDaemonLivenessBaseline(daemonLivenessStatePath(townRoot))
	if !ok {
		t.Fatal("expected a baseline to be recorded after the first run")
	}
	if baseline.HeartbeatCount != 3 {
		t.Errorf("recorded HeartbeatCount = %d, want 3", baseline.HeartbeatCount)
	}
}

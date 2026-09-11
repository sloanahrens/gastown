package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// recorded by "a previous run at least 6m earlier".
func writeLivenessBaseline(t *testing.T, townRoot string, count int64, recordedAt time.Time) {
	t.Helper()
	path := daemonLivenessStatePath(townRoot)
	if err := writeDaemonLivenessBaseline(path, daemonLivenessBaseline{HeartbeatCount: count, RecordedAt: recordedAt}); err != nil {
		t.Fatalf("writeDaemonLivenessBaseline: %v", err)
	}
}

func TestDaemonLivenessCheck_FreshAdvancing_OK(t *testing.T) {
	townRoot := t.TempDir()
	writeLivenessBaseline(t, townRoot, 5, time.Now().Add(-10*time.Minute))
	writeLivenessState(t, townRoot, time.Now().Add(-1*time.Minute), 6)

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
	}
}

func TestDaemonLivenessCheck_FreshStatic_Error(t *testing.T) {
	townRoot := t.TempDir()
	writeLivenessBaseline(t, townRoot, 5, time.Now().Add(-10*time.Minute))
	writeLivenessState(t, townRoot, time.Now().Add(-1*time.Minute), 5)

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError; message: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "count not advancing") {
		t.Errorf("Message = %q, want it to name %q", result.Message, "count not advancing")
	}
}

func TestDaemonLivenessCheck_FreshStatic_BaselineTooYoung_OK(t *testing.T) {
	// The baseline exists but hasn't had a fair chance to advance yet
	// (< staleThreshold old) — must not be flagged as stuck.
	townRoot := t.TempDir()
	writeLivenessBaseline(t, townRoot, 5, time.Now().Add(-1*time.Minute))
	writeLivenessState(t, townRoot, time.Now().Add(-1*time.Minute), 5)

	result := NewDaemonLivenessCheck().Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
	}
}

func TestDaemonLivenessCheck_Stale_Error(t *testing.T) {
	townRoot := t.TempDir()
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

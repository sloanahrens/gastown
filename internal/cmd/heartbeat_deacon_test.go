package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/deacon"
)

// setupDeaconHeartbeatTestTown creates a temp town directory that
// detectTownRootFromCwd can find via the GT_TOWN_ROOT env var fallback
// (mirrors the pattern used in handoff_test.go), and points cwd somewhere
// outside it so cwd-based discovery can't accidentally pass.
func setupDeaconHeartbeatTestTown(t *testing.T) string {
	t.Helper()

	tmpTown := t.TempDir()
	mayorDir := filepath.Join(tmpTown, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("creating mayor dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(`{"name": "test-town"}`), 0644); err != nil {
		t.Fatalf("creating town.json: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting cwd: %v", err)
	}
	if err := os.Chdir(os.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	return tmpTown
}

func withEnv(t *testing.T, key, value string) {
	t.Helper()
	orig, hadOrig := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("setting %s: %v", key, err)
	}
	t.Cleanup(func() {
		if hadOrig {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestTouchDeaconHeartbeat_WritesHeartbeatForDeaconRole(t *testing.T) {
	tmpTown := setupDeaconHeartbeatTestTown(t)
	withEnv(t, "GT_TOWN_ROOT", tmpTown)
	withEnv(t, "GT_ROOT", "")
	withEnv(t, "GT_ROLE", "deacon")

	touchDeaconHeartbeat()

	hb := deacon.ReadHeartbeat(tmpTown)
	if hb == nil {
		t.Fatal("expected deacon heartbeat file to be written")
	}
}

func TestTouchDeaconHeartbeat_SkipsNonDeaconRoles(t *testing.T) {
	tmpTown := setupDeaconHeartbeatTestTown(t)
	withEnv(t, "GT_TOWN_ROOT", tmpTown)
	withEnv(t, "GT_ROOT", "")
	withEnv(t, "GT_ROLE", "gastown/polecats/diamond")

	touchDeaconHeartbeat()

	if hb := deacon.ReadHeartbeat(tmpTown); hb != nil {
		t.Fatalf("expected no heartbeat for non-deacon role, got %+v", hb)
	}
}

func TestTouchDeaconHeartbeat_SkipsWhenPaused(t *testing.T) {
	tmpTown := setupDeaconHeartbeatTestTown(t)
	withEnv(t, "GT_TOWN_ROOT", tmpTown)
	withEnv(t, "GT_ROOT", "")
	withEnv(t, "GT_ROLE", "deacon")

	if err := deacon.Pause(tmpTown, "maintenance", "test"); err != nil {
		t.Fatalf("pausing deacon: %v", err)
	}

	touchDeaconHeartbeat()

	if hb := deacon.ReadHeartbeat(tmpTown); hb != nil {
		t.Fatalf("expected no heartbeat while paused, got %+v", hb)
	}
}

func TestTouchDeaconHeartbeat_RefreshesOnRepeatedCalls(t *testing.T) {
	tmpTown := setupDeaconHeartbeatTestTown(t)
	withEnv(t, "GT_TOWN_ROOT", tmpTown)
	withEnv(t, "GT_ROOT", "")
	withEnv(t, "GT_ROLE", "deacon")

	touchDeaconHeartbeat()
	first := deacon.ReadHeartbeat(tmpTown)
	if first == nil {
		t.Fatal("expected first heartbeat to be written")
	}

	touchDeaconHeartbeat()
	second := deacon.ReadHeartbeat(tmpTown)
	if second == nil {
		t.Fatal("expected second heartbeat to be written")
	}
	if !second.Timestamp.After(first.Timestamp) && second.Cycle <= first.Cycle {
		t.Fatalf("expected heartbeat to advance: first=%+v second=%+v", first, second)
	}
}

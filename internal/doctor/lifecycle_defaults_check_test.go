package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/daemon"
)

func TestLifecycleDefaultsCheck_NoConfig(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	ctx := &CheckContext{TownRoot: tmpDir}

	check := NewLifecycleDefaultsCheck()
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected Warning for missing daemon patrol config, got %s", result.Status)
	}
	if result.Message != "mayor/daemon.json not found" {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

// TestLifecycleDefaultsCheck_TwoFileTownNamesTheDaemonSection: once gt config
// migrate has run mayor/daemon.json no longer exists, so the check must send
// the operator to the "daemon" section of settings/config.json instead.
func TestLifecycleDefaultsCheck_TwoFileTownNamesTheDaemonSection(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDoctorTownFile(t, town, "mayor/town.json", `{"registry":{}}`)
	writeDoctorTownFile(t, town, "settings/config.json", `{"daemon":{"type":"daemon-patrol-config","version":1}}`)

	result := NewLifecycleDefaultsCheck().Run(&CheckContext{TownRoot: town})

	if result.Status != StatusWarning {
		t.Fatalf("status = %s, want Warning for a daemon section with no patrols", result.Status)
	}
	want := `settings/config.json (section "daemon") missing patrols section`
	if result.Message != want {
		t.Errorf("message = %q, want %q", result.Message, want)
	}
	if strings.Contains(result.Message, "daemon.json") {
		t.Errorf("message %q names the retired daemon.json", result.Message)
	}
}

func TestLifecycleDefaultsCheck_FullyConfigured(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	mayorDir := filepath.Join(tmpDir, "mayor")
	os.MkdirAll(mayorDir, 0755)

	config := daemon.DefaultLifecycleConfig()
	data, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(mayorDir, "daemon.json"), data, 0644)

	ctx := &CheckContext{TownRoot: tmpDir}
	check := NewLifecycleDefaultsCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected OK for fully configured, got %s: %s", result.Status, result.Message)
	}
}

func TestLifecycleDefaultsCheck_MissingPatrols(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	mayorDir := filepath.Join(tmpDir, "mayor")
	os.MkdirAll(mayorDir, 0755)

	// Only wisp_reaper configured — rest missing
	config := &daemon.DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &daemon.PatrolsConfig{
			WispReaper: &daemon.WispReaperConfig{Enabled: true, IntervalStr: "30m"},
		},
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(mayorDir, "daemon.json"), data, 0644)

	ctx := &CheckContext{TownRoot: tmpDir}
	check := NewLifecycleDefaultsCheck()
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected Warning for partial config, got %s", result.Status)
	}
	// Should report 6 missing: compactor_dog, checkpoint_dog, doctor_dog, jsonl_git_backup, scheduled_maintenance, handler
	if len(check.missing) != 6 {
		t.Errorf("expected 6 missing patrols, got %d: %v", len(check.missing), check.missing)
	}
}

func TestLifecycleDefaultsCheck_Fix(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	mayorDir := filepath.Join(tmpDir, "mayor")
	os.MkdirAll(mayorDir, 0755)

	// Partial config
	config := &daemon.DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &daemon.PatrolsConfig{
			WispReaper: &daemon.WispReaperConfig{Enabled: true, IntervalStr: "1h"},
		},
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(mayorDir, "daemon.json"), data, 0644)

	ctx := &CheckContext{TownRoot: tmpDir}
	check := NewLifecycleDefaultsCheck()

	// Verify it detects the problem
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected Warning before fix, got %s", result.Status)
	}

	// Fix it
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify fix worked
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected OK after fix, got %s: %s", result.Status, result.Message)
	}

	// Verify user config was preserved
	loaded := daemon.LoadPatrolConfig(tmpDir)
	if loaded.Patrols.WispReaper.IntervalStr != "1h" {
		t.Errorf("expected preserved user interval 1h, got %s", loaded.Patrols.WispReaper.IntervalStr)
	}
}

func TestLifecycleDefaultsCheck_CanFix(t *testing.T) {
	t.Parallel()
	check := NewLifecycleDefaultsCheck()
	if !check.CanFix() {
		t.Error("expected CanFix() to return true")
	}
}

func TestLifecycleDefaultsCheck_NilPatrolsSection(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	mayorDir := filepath.Join(tmpDir, "mayor")
	os.MkdirAll(mayorDir, 0755)

	// Config with no patrols section at all
	config := &daemon.DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(mayorDir, "daemon.json"), data, 0644)

	ctx := &CheckContext{TownRoot: tmpDir}
	check := NewLifecycleDefaultsCheck()
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected Warning for nil patrols, got %s", result.Status)
	}
	if result.Message != "mayor/daemon.json missing patrols section" {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

// TestLifecycleDefaultsCheck_MissingCheckpointDogAndHandler: EnsureLifecycleDefaults
// fills checkpoint_dog and handler, so a town missing them must not read as
// fully configured.
func TestLifecycleDefaultsCheck_MissingCheckpointDogAndHandler(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	mayorDir := filepath.Join(tmpDir, "mayor")
	os.MkdirAll(mayorDir, 0755)

	config := daemon.DefaultLifecycleConfig()
	config.Patrols.CheckpointDog = nil
	config.Patrols.Handler = nil
	data, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(mayorDir, "daemon.json"), data, 0644)

	result := NewLifecycleDefaultsCheck().Run(&CheckContext{TownRoot: tmpDir})

	if result.Status == StatusOK {
		t.Fatalf("status = OK for a config missing checkpoint_dog and handler: %s", result.Message)
	}
	for _, name := range []string{"checkpoint_dog", "handler"} {
		if !strings.Contains(result.Message, name) {
			t.Errorf("message %q does not name %s", result.Message, name)
		}
	}
}

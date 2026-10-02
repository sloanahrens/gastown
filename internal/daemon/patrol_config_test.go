package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

func TestLoadPatrolConfig(t *testing.T) {
	t.Parallel()
	// Create a temp dir with test config
	tmpDir := t.TempDir()
	mayorDir := filepath.Join(tmpDir, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write test config
	configJSON := `{
		"type": "daemon-patrol-config",
		"version": 1,
		"patrols": {
			"mayor": {"enabled": false},
			"witness": {"enabled": false}
		}
	}`
	if err := os.WriteFile(filepath.Join(mayorDir, "daemon.json"), []byte(configJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// Load config
	config := LoadPatrolConfig(tmpDir)
	if config == nil {
		t.Fatal("expected config to be loaded")
	}

	// Test enabled flags
	if IsPatrolEnabled(config, "mayor") {
		t.Error("expected mayor to be disabled")
	}
	if !IsPatrolEnabled(config, "handler") {
		t.Error("expected handler to be enabled (default)")
	}
}

func TestIsPatrolEnabled_NilConfig(t *testing.T) {
	t.Parallel()
	// Should default to enabled when config is nil
	if !IsPatrolEnabled(nil, "refinery") {
		t.Error("expected default to be enabled")
	}
}

func TestSaveAndLoadPatrolConfig(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	config := &DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &PatrolsConfig{
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled: true,
				Window:  "03:00",
			},
		},
	}

	// Save
	if err := SavePatrolConfig(tmpDir, config); err != nil {
		t.Fatalf("SavePatrolConfig failed: %v", err)
	}

	// Load back
	loaded := LoadPatrolConfig(tmpDir)
	if loaded == nil {
		t.Fatal("expected config to be loaded")
	}

	if !IsPatrolEnabled(loaded, "scheduled_maintenance") {
		t.Error("expected scheduled_maintenance to be enabled")
	}
	sm := loaded.Patrols.ScheduledMaintenance
	if sm.Window != "03:00" {
		t.Errorf("expected window 03:00, got %q", sm.Window)
	}
}

func TestLoadDisabledPatrolsFromTownSettings(t *testing.T) {
	t.Parallel()
	// No settings file: returns nil
	tmpDir := t.TempDir()
	got := loadDisabledPatrolsFromTownSettings(tmpDir)
	if got != nil {
		t.Errorf("expected nil for missing settings, got %v", got)
	}

	// Empty disabled_patrols: returns nil
	settingsDir := filepath.Join(tmpDir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(`{
		"type": "town-settings", "version": 1
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	got = loadDisabledPatrolsFromTownSettings(tmpDir)
	if got != nil {
		t.Errorf("expected nil for empty disabled_patrols, got %v", got)
	}

	// With disabled patrols
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(`{
		"type": "town-settings", "version": 1,
		"disabled_patrols": ["doctor_dog", "compactor_dog"]
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	got = loadDisabledPatrolsFromTownSettings(tmpDir)
	if len(got) != 2 {
		t.Fatalf("expected 2 disabled patrols, got %d", len(got))
	}
	if !got["doctor_dog"] {
		t.Error("expected doctor_dog to be disabled")
	}
	if !got["compactor_dog"] {
		t.Error("expected compactor_dog to be disabled")
	}
	if got["witness"] {
		t.Error("expected witness to NOT be disabled")
	}
}

// TestLoadDisabledPatrolsFromTownSettings_UnknownKeyIsReported pins the
// fail-closed read: a key the town settings type does not declare is not
// silently read as a town with nothing disabled (gt-y3pgh.2.7).
func TestLoadDisabledPatrolsFromTownSettings_UnknownKeyIsReported(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	settingsDir := filepath.Join(tmpDir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(settingsDir, "config.json")
	if err := os.WriteFile(settingsPath, []byte(`{
		"type": "town-settings", "version": 1,
		"disabled_patrols": ["witness"],
		"undeclared_key": true
	}`), 0644); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	got := loadDisabledPatrolsFromTownSettingsTo(tmpDir, &logged)

	if got != nil {
		t.Errorf("expected nil for a file with an undeclared key, got %v", got)
	}
	line := strings.TrimSuffix(logged.String(), "\n")
	if line == "" {
		t.Fatal("expected one logged line naming the file, got none")
	}
	if n := strings.Count(line, "\n") + 1; n != 1 {
		t.Errorf("expected one logged line, got %d:\n%s", n, line)
	}
	if !strings.Contains(line, settingsPath) {
		t.Errorf("logged line does not name %s: %q", settingsPath, line)
	}
}

func TestIsPatrolActive(t *testing.T) {
	t.Parallel()
	// Patrol enabled in daemon config, not in disabled list → active
	d := &Daemon{
		patrolConfig:    nil, // nil config = all default-enabled patrols enabled
		disabledPatrols: nil,
	}
	if !d.isPatrolActive("handler") {
		t.Error("expected handler to be active with nil configs")
	}

	// Patrol enabled in daemon config, but in disabled list → inactive
	d.disabledPatrols = map[string]bool{"handler": true}
	if d.isPatrolActive("handler") {
		t.Error("expected handler to be inactive when in disabled list")
	}

	// Patrol disabled in daemon config, not in disabled list → inactive
	d.disabledPatrols = nil
	d.patrolConfig = &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			Handler: &PatrolConfig{Enabled: false},
		},
	}
	if d.isPatrolActive("handler") {
		t.Error("expected handler to be inactive when disabled in daemon config")
	}

	// Opt-in patrol (doctor_dog) disabled by default, in disabled list → inactive
	d.patrolConfig = nil
	d.disabledPatrols = map[string]bool{"doctor_dog": true}
	if d.isPatrolActive("doctor_dog") {
		t.Error("expected doctor_dog to be inactive")
	}

	// Opt-in patrol enabled in daemon config but in disabled list → disabled wins
	d.patrolConfig = &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			DoctorDog: &DoctorDogConfig{Enabled: true},
		},
	}
	d.disabledPatrols = map[string]bool{"doctor_dog": true}
	if d.isPatrolActive("doctor_dog") {
		t.Error("expected doctor_dog to be inactive when in disabled list, even if enabled in daemon config")
	}
}

// The dolt_remotes patrol is gone (ADR 0002). Strict decoding keeps the
// key only as opaque retired data (json.RawMessage), so it has no patrol
// type, no Enabled switch, and nothing can turn a Dolt remote push back on
// from daemon.json (gt-y3pgh.1).
func TestPatrolsConfig_DoltRemotesKeyIsOpaqueRetiredData(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(PatrolsConfig{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if tag := f.Tag.Get("json"); strings.HasPrefix(tag, "dolt_remotes") && f.Type != reflect.TypeOf(json.RawMessage(nil)) {
			t.Fatalf("PatrolsConfig.%s maps dolt_remotes to %s, want json.RawMessage", f.Name, f.Type)
		}
	}
}

// A daemon.json written before the removals still carries dolt_remotes and
// dolt_backup blocks. It must load cleanly, keeping them verbatim.
func TestLoadPatrolConfig_IgnoresLegacyDoltRemotesKey(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"type":"daemon-patrol-config","version":1,"patrols":{"dolt_remotes":{"enabled":true,"interval":900000000000},"dolt_backup":{"enabled":true}}}`
	if err := os.WriteFile(PatrolConfigFile(town), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := LoadPatrolConfig(town)
	if cfg == nil || cfg.Patrols == nil || string(cfg.Patrols.DoltBackup) != `{"enabled":true}` {
		t.Fatalf("legacy config with dolt_remotes failed to load: %+v", cfg)
	}
}

// TestPatrolConfigSource_FollowsTheDaemonSectionToItsHost: on the two-file
// layout the daemon's patrols live in settings/config.json under "daemon", and
// the startup line must name that file, not the retired mayor/daemon.json
// (gt-y3pgh.12).
func TestPatrolConfigSource_FollowsTheDaemonSectionToItsHost(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The two-file marker: mayor/town.json carries the registry section.
	townJSON := `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","registry":{"rigs":{}}}`
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte(townJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	settings := `{"type":"town-settings","version":1,"daemon":{"type":"daemon-patrol-config","version":1,"patrols":{"mayor":{"enabled":false}}}}`
	if err := os.WriteFile(filepath.Join(town, "settings", "config.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	got := PatrolConfigSource(town)
	want := filepath.Join(town, "settings", "config.json")
	if !strings.HasPrefix(got, want) || !strings.Contains(got, `section "daemon"`) {
		t.Errorf("PatrolConfigSource = %q, want %s and its daemon section", got, want)
	}
	// The config still loads through the retired path, so the log and the load
	// name one file.
	if cfg := LoadPatrolConfig(town); cfg == nil || cfg.Patrols == nil {
		t.Fatalf("patrol config did not load through the section: %+v", cfg)
	}

	// A town that still has mayor/daemon.json keeps that name.
	fiveFile := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fiveFile, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PatrolConfigFile(fiveFile), []byte(`{"type":"daemon-patrol-config","version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := PatrolConfigSource(fiveFile); got != PatrolConfigFile(fiveFile) {
		t.Errorf("PatrolConfigSource on a five-file town = %q, want %q", got, PatrolConfigFile(fiveFile))
	}
}

// Shutdown no longer pushes Dolt remotes or flushes OTel, so its budget is the
// Dolt server's graceful stop and nothing else.
func TestShutdownBudget_HasNoRemotePushStep(t *testing.T) {
	t.Parallel()
	if ShutdownBudget != doltServerStopBudget {
		t.Fatalf("ShutdownBudget = %v, want %v (Dolt stop only)",
			ShutdownBudget, doltServerStopBudget)
	}
}

func TestIsPatrolActiveMayorKnob(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	if !d.isPatrolActive(constants.RoleMayor) {
		t.Fatal("mayor supervision must default to on with no config")
	}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{Mayor: &PatrolConfig{Enabled: false}}}
	if d.isPatrolActive(constants.RoleMayor) {
		t.Fatal("patrols.mayor.enabled=false must turn mayor supervision off")
	}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{Mayor: &PatrolConfig{Enabled: true}}}
	if !d.isPatrolActive(constants.RoleMayor) {
		t.Fatal("patrols.mayor.enabled=true must keep mayor supervision on")
	}
}

// ABOUTME: Tests for global state management.
// ABOUTME: Verifies enable/disable toggle and XDG path resolution.

package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const testHome = "/home/tester"

// testEnviron returns an environ over vars with a fixed home directory.
func testEnviron(vars map[string]string) environ {
	return environ{
		getenv:  func(k string) string { return vars[k] },
		homeDir: func() (string, error) { return testHome, nil },
	}
}

func TestStateDir(t *testing.T) {
	t.Parallel()
	if got, want := testEnviron(nil).stateDir(), filepath.Join(testHome, ".local", "state", "gastown"); got != want {
		t.Errorf("stateDir() = %q, want %q", got, want)
	}
	e := testEnviron(map[string]string{"XDG_STATE_HOME": "/custom/state"})
	if got := filepath.ToSlash(e.stateDir()); got != "/custom/state/gastown" {
		t.Errorf("stateDir() with XDG = %q, want /custom/state/gastown", got)
	}
}

func TestConfigDir(t *testing.T) {
	t.Parallel()
	if got, want := testEnviron(nil).configDir(), filepath.Join(testHome, ".config", "gastown"); got != want {
		t.Errorf("configDir() = %q, want %q", got, want)
	}
	e := testEnviron(map[string]string{"XDG_CONFIG_HOME": "/custom/config"})
	if got := filepath.ToSlash(e.configDir()); got != "/custom/config/gastown" {
		t.Errorf("configDir() with XDG = %q, want /custom/config/gastown", got)
	}
}

func TestCacheDir(t *testing.T) {
	t.Parallel()
	if got, want := testEnviron(nil).cacheDir(), filepath.Join(testHome, ".cache", "gastown"); got != want {
		t.Errorf("cacheDir() = %q, want %q", got, want)
	}
	e := testEnviron(map[string]string{"XDG_CACHE_HOME": "/custom/cache"})
	if got := filepath.ToSlash(e.cacheDir()); got != "/custom/cache/gastown" {
		t.Errorf("cacheDir() with XDG = %q, want /custom/cache/gastown", got)
	}
}

// TestPublicDirsReadProcessEnv checks the exported wrappers are wired to the
// process environment, without mutating it.
func TestPublicDirsReadProcessEnv(t *testing.T) {
	t.Parallel()
	e := osEnviron()
	if got, want := StateDir(), e.stateDir(); got != want {
		t.Errorf("StateDir() = %q, want %q", got, want)
	}
	if got, want := ConfigDir(), e.configDir(); got != want {
		t.Errorf("ConfigDir() = %q, want %q", got, want)
	}
	if got, want := CacheDir(), e.cacheDir(); got != want {
		t.Errorf("CacheDir() = %q, want %q", got, want)
	}
	if got, want := StatePath(), filepath.Join(e.stateDir(), "state.json"); got != want {
		t.Errorf("StatePath() = %q, want %q", got, want)
	}
}

func TestIsEnabled_EnvOverride(t *testing.T) {
	t.Parallel()
	// XDG_STATE_HOME points at an empty dir so the state file cannot decide.
	dir := t.TempDir()
	if testEnviron(map[string]string{"XDG_STATE_HOME": dir, "GASTOWN_DISABLED": "1"}).isEnabled() {
		t.Error("isEnabled() should return false when GASTOWN_DISABLED=1")
	}
	if !testEnviron(map[string]string{"XDG_STATE_HOME": dir, "GASTOWN_ENABLED": "1"}).isEnabled() {
		t.Error("isEnabled() should return true when GASTOWN_ENABLED=1")
	}
}

func TestIsEnabled_DisabledOverridesEnabled(t *testing.T) {
	t.Parallel()
	e := testEnviron(map[string]string{"XDG_STATE_HOME": t.TempDir(), "GASTOWN_DISABLED": "1", "GASTOWN_ENABLED": "1"})
	if e.isEnabled() {
		t.Error("GASTOWN_DISABLED should take precedence over GASTOWN_ENABLED")
	}
}

func TestEnableDisable(t *testing.T) {
	t.Parallel()
	e := testEnviron(map[string]string{"XDG_STATE_HOME": t.TempDir()})

	if e.isEnabled() {
		t.Error("isEnabled() should default to false with no state file")
	}

	if err := e.enable("1.0.0"); err != nil {
		t.Fatalf("enable() failed: %v", err)
	}

	if !e.isEnabled() {
		t.Error("isEnabled() should return true after enable()")
	}

	s, err := e.load()
	if err != nil {
		t.Fatalf("load() failed: %v", err)
	}
	if s.Version != "1.0.0" {
		t.Errorf("State.Version = %q, want %q", s.Version, "1.0.0")
	}
	if s.MachineID == "" {
		t.Error("State.MachineID should not be empty")
	}
	if got := e.getMachineID(); got != s.MachineID {
		t.Errorf("getMachineID() = %q, want stored %q", got, s.MachineID)
	}
	info, err := os.Stat(e.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("state file perm = %o, want 600", perm)
	}

	if err := e.disable(); err != nil {
		t.Fatalf("disable() failed: %v", err)
	}

	if e.isEnabled() {
		t.Error("isEnabled() should return false after disable()")
	}
	if s2, err := e.load(); err != nil || s2.MachineID != s.MachineID {
		t.Errorf("after disable() load() = %+v, %v; want machine id %q kept", s2, err, s.MachineID)
	}
}

func TestDisableWithoutState(t *testing.T) {
	t.Parallel()
	e := testEnviron(map[string]string{"XDG_STATE_HOME": t.TempDir()})
	if err := e.disable(); err != nil {
		t.Fatalf("disable() failed: %v", err)
	}
	s, err := e.load()
	if err != nil {
		t.Fatalf("load() failed: %v", err)
	}
	if s.Enabled || s.MachineID == "" {
		t.Errorf("state = %+v, want disabled with a machine id", s)
	}
}

func TestShellIntegrationAndDoctorRun(t *testing.T) {
	t.Parallel()
	e := testEnviron(map[string]string{"XDG_STATE_HOME": t.TempDir()})

	if err := e.recordDoctorRun(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("recordDoctorRun() with no state = %v, want ErrNotExist", err)
	}
	if got := e.getMachineID(); len(got) != 8 {
		t.Errorf("getMachineID() with no state = %q, want a fresh 8-char id", got)
	}

	if err := e.setShellIntegration("zsh"); err != nil {
		t.Fatalf("setShellIntegration() failed: %v", err)
	}
	if err := e.recordDoctorRun(); err != nil {
		t.Fatalf("recordDoctorRun() failed: %v", err)
	}
	s, err := e.load()
	if err != nil {
		t.Fatalf("load() failed: %v", err)
	}
	if s.ShellIntegration != "zsh" {
		t.Errorf("ShellIntegration = %q, want zsh", s.ShellIntegration)
	}
	if s.LastDoctorRun.IsZero() {
		t.Error("LastDoctorRun not recorded")
	}
}

func TestLoadRejectsCorruptState(t *testing.T) {
	t.Parallel()
	e := testEnviron(map[string]string{"XDG_STATE_HOME": t.TempDir()})
	if err := os.MkdirAll(e.stateDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.statePath(), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.load(); err == nil {
		t.Error("load() of corrupt state = nil error, want a decode error")
	}
	if e.isEnabled() {
		t.Error("isEnabled() with corrupt state should default to false")
	}
}

func TestGenerateMachineID(t *testing.T) {
	t.Parallel()
	id1 := generateMachineID()
	id2 := generateMachineID()

	if len(id1) != 8 {
		t.Errorf("generateMachineID() length = %d, want 8", len(id1))
	}
	if id1 == id2 {
		t.Error("generateMachineID() should generate unique IDs")
	}
}

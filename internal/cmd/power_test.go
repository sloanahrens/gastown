package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// The login job runs boot-hold so a reboot lands in standby. It never touches
// the Forgejo stack, so a town settings file that fails to decode must not stop
// it from writing the hold (gt-g4xe9).
func TestBootHoldWithMalformedSettings(t *testing.T) {
	townRoot := setupTestTownForConfig(t)
	settingsPath := config.TownSettingsPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(townRoot)

	if _, err := config.ForgejoDir(townRoot); err == nil {
		t.Fatal("precondition: the malformed settings file should fail to decode")
	}

	if err := powerBootHoldCmd.RunE(powerBootHoldCmd, nil); err != nil {
		t.Fatalf("boot-hold: %v", err)
	}
	if _, err := os.Stat(dispatch.HoldFilePath(townRoot)); err != nil {
		t.Fatalf("hold file not written: %v", err)
	}
}

// The states that do reach the Forgejo stack still report a settings file they
// cannot read.
func TestPowerOptionsForgejoSettingsError(t *testing.T) {
	townRoot := setupTestTownForConfig(t)
	settingsPath := config.TownSettingsPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(townRoot)

	if _, err := powerOptions(true); err == nil {
		t.Error("powerOptions(true) accepted malformed settings")
	}
	if _, err := powerOptions(false); err != nil {
		t.Errorf("powerOptions(false): %v", err)
	}
}

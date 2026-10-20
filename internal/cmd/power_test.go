package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// townWithMalformedSettings is a town whose settings/config.json fails to
// decode.
func townWithMalformedSettings(t *testing.T) string {
	t.Helper()
	townRoot := setupTestTownForConfig(t)
	settingsPath := config.TownSettingsPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.ForgejoDir(townRoot); err == nil {
		t.Fatal("precondition: the malformed settings file should fail to decode")
	}
	return townRoot
}

// The login job runs boot-hold so a reboot lands in standby. It never touches
// the Forgejo stack, so a town settings file that fails to decode must not stop
// it from writing the hold (gt-g4xe9). This runs the command's own RunE, so
// wiring boot-hold back to the settings-reading builder fails here.
func TestBootHoldCommandWithMalformedSettings(t *testing.T) {
	t.Parallel()
	townRoot := townWithMalformedSettings(t)

	cmd := &cobra.Command{}
	cmd.SetContext(withPowerTownRoot(context.Background(), townRoot))
	if err := powerBootHoldCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("boot-hold: %v", err)
	}
	if _, err := os.Stat(dispatch.HoldFilePath(townRoot)); err != nil {
		t.Fatalf("hold file not written: %v", err)
	}
}

// The states that do reach the Forgejo stack still report a settings file they
// cannot read; boot-hold is the one state that runs without one.
func TestPowerOptionsForgejoSettingsError(t *testing.T) {
	t.Parallel()
	townRoot := townWithMalformedSettings(t)

	if _, err := powerOptionsFor(townRoot); err == nil {
		t.Error("powerOptionsFor accepted malformed settings")
	}
	if _, err := powerBootHoldOptionsFor(townRoot); err != nil {
		t.Errorf("powerBootHoldOptionsFor: %v", err)
	}
}

// A state on powerOptionsFor always gets a Forgejo dir, so none can read an
// empty path silently.
func TestPowerOptionsForgejoDirResolved(t *testing.T) {
	t.Parallel()
	townRoot := setupTestTownForConfig(t)

	o, err := powerOptionsFor(townRoot)
	if err != nil {
		t.Fatalf("powerOptionsFor: %v", err)
	}
	if o.ForgejoDir == "" {
		t.Error("powerOptionsFor returned an empty ForgejoDir")
	}
}

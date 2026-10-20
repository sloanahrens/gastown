package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/power"
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
// it from writing the hold (gt-g4xe9).
func TestBootHoldWithMalformedSettings(t *testing.T) {
	t.Parallel()
	townRoot := townWithMalformedSettings(t)

	o, err := powerOptionsFor(townRoot, powerBootHoldNeedsForgejo)
	if err != nil {
		t.Fatalf("powerOptionsFor: %v", err)
	}
	o.Out, o.ErrOut = io.Discard, io.Discard
	if err := power.BootHold(context.Background(), o); err != nil {
		t.Fatalf("boot-hold: %v", err)
	}
	if _, err := os.Stat(dispatch.HoldFilePath(townRoot)); err != nil {
		t.Fatalf("hold file not written: %v", err)
	}
}

// The states that do reach the Forgejo stack still report a settings file they
// cannot read.
func TestPowerOptionsForgejoSettingsError(t *testing.T) {
	t.Parallel()
	townRoot := townWithMalformedSettings(t)

	if _, err := powerOptionsFor(townRoot, true); err == nil {
		t.Error("powerOptionsFor(needsForgejo) accepted malformed settings")
	}
	if _, err := powerOptionsFor(townRoot, false); err != nil {
		t.Errorf("powerOptionsFor(no forgejo): %v", err)
	}
}

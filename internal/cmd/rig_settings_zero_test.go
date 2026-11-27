package cmd

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestRigSettingsSetZeroValue is the gt-ltv1vm regression guard: omitempty drops
// a zero-valued key from the re-marshaled settings, which must not make a
// declared field read as unknown.
func TestRigSettingsSetZeroValue(t *testing.T) {
	t.Parallel()
	townRoot, rigName := setupTestRigForSettings(t)
	settingsPath := filepath.Join(townRoot, rigName, "settings", "config.json")
	settings := &config.RigSettings{Landing: &config.LandingConfig{Followups: true}}
	if err := config.SaveRigSettings(settingsPath, settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	err := rigSettingsSet(townRigCmdEnv(townRoot, io.Discard, io.Discard), []string{rigName, "landing.followups", "false"})
	if err != nil {
		t.Fatalf("setting landing.followups=false: %v", err)
	}
	loaded, err := config.LoadRigSettings(settingsPath)
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if loaded.Landing == nil || loaded.Landing.Followups {
		t.Errorf("Landing = %+v after set, want Followups false", loaded.Landing)
	}
}

// TestRigSettingsSetUnknownKey holds the refusal: a key the settings type does
// not declare is refused at any depth.
func TestRigSettingsSetUnknownKey(t *testing.T) {
	t.Parallel()
	for _, keyPath := range []string{"landing.nope", "something_else"} {
		t.Run(keyPath, func(t *testing.T) {
			t.Parallel()
			townRoot, rigName := setupTestRigForSettings(t)
			err := rigSettingsSet(townRigCmdEnv(townRoot, io.Discard, io.Discard), []string{rigName, keyPath, "false"})
			if err == nil {
				t.Fatalf("setting %s: expected an unknown key error", keyPath)
			}
			if want := `unknown key "` + keyPath + `"`; !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to contain %q", err, want)
			}
		})
	}
}

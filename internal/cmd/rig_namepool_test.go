package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
)

func namepoolTestManager(t *testing.T, townRoot string) *rig.Manager {
	t.Helper()
	return rig.NewManager(townRoot, &config.RigsConfig{Rigs: map[string]config.RigEntry{}}, nil)
}

func TestAutoAssignNamepoolTheme_UnparseableSettingsLeftUntouched(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
	}{
		{"retired key", `{"type":"rig-settings","version":1,"merge_queue":{"run_tests":true},"agents":{"x":{"command":"y"}}}`},
		{"bad json", `{"type":"rig-settings",`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			settingsPath := filepath.Join(townRoot, "myrig", "settings", "config.json")
			if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(settingsPath, []byte(tt.content), 0644); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			autoAssignNamepoolTheme(&out, townRoot, "myrig", namepoolTestManager(t, townRoot))

			got, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, []byte(tt.content)) {
				t.Errorf("settings rewritten:\n got %s\nwant %s", got, tt.content)
			}
			for _, want := range []string{"Could not load " + settingsPath, "leaving it untouched"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("warning missing %q; output: %q", want, out.String())
				}
			}
			if strings.Contains(out.String(), "auto-assigned") {
				t.Errorf("reported an assignment for an untouched file: %q", out.String())
			}
		})
	}
}

func TestAutoAssignNamepoolTheme_MissingSettingsGetsStyle(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	var out bytes.Buffer
	autoAssignNamepoolTheme(&out, townRoot, "myrig", namepoolTestManager(t, townRoot))

	settings, err := config.LoadRigSettings(filepath.Join(townRoot, "myrig", "settings", "config.json"))
	if err != nil {
		t.Fatalf("LoadRigSettings: %v", err)
	}
	if settings.Namepool == nil || settings.Namepool.Style == "" {
		t.Fatalf("namepool style not assigned: %+v", settings.Namepool)
	}
	if want := "Namepool theme: " + settings.Namepool.Style; !strings.Contains(out.String(), want) {
		t.Errorf("output missing %q: %q", want, out.String())
	}
}

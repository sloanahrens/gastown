package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
)

// The Rigs panel's Names column is read from each rig's own settings, and the
// samples it carries are capped, so one rig's theme file cannot push the
// tooltip past a line (gt-yieek).
func TestDashRigThemesReadEachRigsOwnTheme(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	settings := func(rigName string, pool *config.NamepoolConfig) {
		t.Helper()
		path := filepath.Join(town, rigName, "settings", "config.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("making settings dir: %v", err)
		}
		s := config.NewRigSettings()
		s.Namepool = pool
		if err := config.SaveRigSettings(path, s); err != nil {
			t.Fatalf("saving settings for %s: %v", rigName, err)
		}
	}

	settings("gastown", &config.NamepoolConfig{Style: "minerals"})
	settings("harbor", &config.NamepoolConfig{Style: "harbor"})
	settings("ghost", &config.NamepoolConfig{Style: "nowhere"})
	settings("legacy", &config.NamepoolConfig{Names: []string{"anchor", "berth"}})

	themesDir := filepath.Join(town, "settings", "themes")
	if err := os.MkdirAll(themesDir, 0o755); err != nil {
		t.Fatalf("making themes dir: %v", err)
	}
	harbor := "anchor\nberth\nchannel\ndock\nferry\nhull\nknot\n"
	if err := os.WriteFile(filepath.Join(themesDir, "harbor.txt"), []byte(harbor), 0o644); err != nil {
		t.Fatalf("writing theme file: %v", err)
	}

	themes := newDashRigThemes(town)
	tests := []struct {
		rig   string
		theme string
		names []string
	}{
		{"gastown", "minerals", []string{"obsidian", "quartz", "jasper", "onyx", "opal"}},
		{"harbor", "harbor", []string{"anchor", "berth", "channel", "dock", "ferry"}},
		// A style with no theme file behind it still names the theme: the
		// column is worth more than the tooltip.
		{"ghost", "nowhere", nil},
		// An explicit name list has no theme to name.
		{"legacy", "", nil},
		{"plain", polecat.ThemeForRig("plain"), nil}, // the hash default, sampled below
	}
	for _, tc := range tests {
		theme, names := themes(tc.rig)
		if theme != tc.theme {
			t.Errorf("%s theme = %q, want %q", tc.rig, theme, tc.theme)
		}
		if tc.names != nil && strings.Join(names, ",") != strings.Join(tc.names, ",") {
			t.Errorf("%s names = %v, want %v", tc.rig, names, tc.names)
		}
		if tc.names == nil && len(names) > rigThemeSamples {
			t.Errorf("%s carries %d names, want at most %d", tc.rig, len(names), rigThemeSamples)
		}
		if tc.rig == "plain" && len(names) != rigThemeSamples {
			t.Errorf("plain names = %v, want the default theme's first %d", names, rigThemeSamples)
		}
	}
}

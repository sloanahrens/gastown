package session

import (
	"os"
	"path/filepath"
	"testing"
)

// writeWindowTintTown builds a town root with a mayor/town.json marker and the
// given rig and global config bodies. An empty body leaves that file absent.
func writeWindowTintTown(t *testing.T, rigConfig, mayorConfig string) string {
	t.Helper()
	townRoot := t.TempDir()
	writeWindowTintFile(t, filepath.Join(townRoot, "mayor", "town.json"), `{}`)
	if rigConfig != "" {
		writeWindowTintFile(t, filepath.Join(townRoot, "testrig", "settings", "config.json"), rigConfig)
	}
	if mayorConfig != "" {
		writeWindowTintFile(t, filepath.Join(townRoot, "mayor", "config.json"), mayorConfig)
	}
	return townRoot
}

func writeWindowTintFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestIsWindowTintEnabled_BlockWithoutEnabledKey: a rig window_tint block that
// omits enabled opts the rig in, the same as ResolveWindowTint reads it.
// Before the fix this returned false and the rig never inherited the tint.
func TestIsWindowTintEnabled_BlockWithoutEnabledKey(t *testing.T) {
	t.Parallel()
	townRoot := writeWindowTintTown(t,
		`{"theme":{"window_tint":{"tint_factor":0.3}}}`, "")

	if !isWindowTintEnabledIn(townRoot, "testrig") {
		t.Error("isWindowTintEnabledIn = false for a rig block with no enabled key, want true")
	}
}

func TestIsWindowTintEnabled_EnabledKeyDecides(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		rigConfig   string
		mayorConfig string
		want        bool
	}{
		{
			name:        "global block without enabled key is opted in",
			mayorConfig: `{"theme":{"window_tint":{"tint_factor":0.3}}}`,
			want:        true,
		},
		{
			name:        "explicit false in rig overrides an opted-in global block",
			rigConfig:   `{"theme":{"window_tint":{"enabled":false}}}`,
			mayorConfig: `{"theme":{"window_tint":{"tint_factor":0.3}}}`,
			want:        false,
		},
		{
			name:        "explicit true in rig enables",
			rigConfig:   `{"theme":{"window_tint":{"enabled":true}}}`,
			mayorConfig: `{"theme":{"window_tint":{"enabled":false}}}`,
			want:        true,
		},
		{
			name: "no window_tint block anywhere stays off",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := writeWindowTintTown(t, tt.rigConfig, tt.mayorConfig)

			if got := isWindowTintEnabledIn(townRoot, "testrig"); got != tt.want {
				t.Errorf("isWindowTintEnabledIn = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsWindowTintEnabled_UnreadableConfigFallsThrough: a config file that
// fails to load is skipped, so the lookup moves on to the next level.
func TestIsWindowTintEnabled_UnreadableConfigFallsThrough(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		rigConfig   string
		mayorConfig string
		want        bool
	}{
		{
			name:        "malformed rig block defers to an opted-in global block",
			rigConfig:   `{"theme":{"window_tint":`,
			mayorConfig: `{"theme":{"window_tint":{"tint_factor":0.3}}}`,
			want:        true,
		},
		{
			name:      "malformed rig block with no global block stays off",
			rigConfig: `{"theme":{"window_tint":`,
			want:      false,
		},
		{
			name:        "malformed global block stays off",
			mayorConfig: `{"theme":`,
			want:        false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := writeWindowTintTown(t, tt.rigConfig, tt.mayorConfig)

			if got := isWindowTintEnabledIn(townRoot, "testrig"); got != tt.want {
				t.Errorf("isWindowTintEnabledIn = %v, want %v", got, tt.want)
			}
		})
	}
}

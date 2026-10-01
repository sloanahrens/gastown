package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedTargetKey(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ role, settingsDir, want string }{
		{"polecat", "/town/gastown/polecats", "gastown/polecats"},
		{"crew", "/town/gastown/crew", "gastown/crew"},
		{"mayor", "/town/mayor", "mayor"},
		{"boot", "/town/deacon/dogs/boot", "boot"},
		{"polecat", "polecats", "polecats"},
	} {
		if got := ManagedTargetKey(tt.role, tt.settingsDir); got != tt.want {
			t.Errorf("ManagedTargetKey(%q, %q) = %q, want %q", tt.role, tt.settingsDir, got, tt.want)
		}
	}
}

// CheckManagedClaudeSettings passes only a file that exists, parses and
// carries the managed hooks for its key (gt-4k3fj.8.3).
func TestCheckManagedClaudeSettings(t *testing.T) {
	t.Parallel()
	home := HomeAt(t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	target := Target{Path: path, Key: "mayor", Role: "mayor", Provider: "claude"}

	if err := home.CheckManagedClaudeSettings(target); err == nil {
		t.Error("missing file passed")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := home.CheckManagedClaudeSettings(target); err == nil {
		t.Error("unparseable file passed")
	}
	if err := os.WriteFile(path, []byte(`{"hooks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := home.CheckManagedClaudeSettings(target); err == nil {
		t.Error("file without the managed hooks passed")
	}
	if _, err := home.SyncManagedClaudeSettings(target, false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := home.CheckManagedClaudeSettings(target); err != nil {
		t.Errorf("synced file failed: %v", err)
	}
}

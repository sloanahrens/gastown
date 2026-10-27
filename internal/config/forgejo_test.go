package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func writeTownSettings(t *testing.T, town, body string) {
	t.Helper()
	dir := filepath.Join(town, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestForgejoDirDefaultsToHomeForgejo(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	got, err := config.ForgejoDirFrom(town, fixedHome("/home/tester"))
	if err != nil {
		t.Fatalf("ForgejoDirFrom: %v", err)
	}
	if want := "/home/tester/forgejo"; got != want {
		t.Errorf("ForgejoDirFrom = %q, want %q", got, want)
	}
}

func TestForgejoDirReadsConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownSettings(t, town, `{"type":"town-settings","version":1,"forgejo":{"dir":"/srv/forgejo"}}`)
	got, err := config.ForgejoDir(town)
	if err != nil {
		t.Fatalf("ForgejoDir: %v", err)
	}
	if want := "/srv/forgejo"; got != want {
		t.Errorf("ForgejoDir = %q, want %q", got, want)
	}
}

func TestForgejoDirExpandsTilde(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownSettings(t, town, `{"type":"town-settings","version":1,"forgejo":{"dir":"~/stacks/forgejo"}}`)
	got, err := config.ForgejoDirFrom(town, fixedHome("/home/tester"))
	if err != nil {
		t.Fatalf("ForgejoDirFrom: %v", err)
	}
	if want := filepath.Join("/home/tester", "stacks", "forgejo"); got != want {
		t.Errorf("ForgejoDirFrom = %q, want %q", got, want)
	}
}

// fixedHome is a home directory for a test's own use, in place of the
// process's.
func fixedHome(home string) func() (string, error) {
	return func() (string, error) { return home, nil }
}

func TestForgejoDirRejectsUnknownKey(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownSettings(t, town, `{"type":"town-settings","version":1,"forgejo":{"directory":"/srv/forgejo"}}`)
	if _, err := config.ForgejoDir(town); err == nil {
		t.Fatal("ForgejoDir accepted an undeclared key; the settings file is strictly decoded")
	}
}

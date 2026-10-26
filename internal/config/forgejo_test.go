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
	//testpolicy:allow no-host-env — the default forgejo dir is under the real home by design; the test asserts that default and reads nothing else
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.ForgejoDir(town)
	if err != nil {
		t.Fatalf("ForgejoDir: %v", err)
	}
	if want := filepath.Join(home, "forgejo"); got != want {
		t.Errorf("ForgejoDir = %q, want %q", got, want)
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
	//testpolicy:allow no-host-env — the ~/stacks path is expanded against the real home; the expansion is the input under test
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.ForgejoDir(town)
	if err != nil {
		t.Fatalf("ForgejoDir: %v", err)
	}
	if want := filepath.Join(home, "stacks", "forgejo"); got != want {
		t.Errorf("ForgejoDir = %q, want %q", got, want)
	}
}

func TestForgejoDirRejectsUnknownKey(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownSettings(t, town, `{"type":"town-settings","version":1,"forgejo":{"directory":"/srv/forgejo"}}`)
	if _, err := config.ForgejoDir(town); err == nil {
		t.Fatal("ForgejoDir accepted an undeclared key; the settings file is strictly decoded")
	}
}

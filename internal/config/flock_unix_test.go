//go:build unix

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A wait-free lock refuses the lock another writer holds, and takes it once
// that writer lets go.
func TestLockConfigFileNoWaitRefusesAHeldLock(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	unlock, err := lockConfigFile(path)
	if err != nil {
		t.Fatalf("lockConfigFile: %v", err)
	}
	if _, err := lockConfigFileNoWait(path); !errors.Is(err, ErrConfigLocked) {
		t.Fatalf("lockConfigFileNoWait with the lock held = %v, want ErrConfigLocked", err)
	}
	unlock()

	unlockNoWait, err := lockConfigFileNoWait(path)
	if err != nil {
		t.Fatalf("lockConfigFileNoWait after unlock = %v, want the lock", err)
	}
	unlockNoWait()
}

// A wait-free write behind a held lock is refused with ErrConfigLocked and
// leaves the file alone.
func TestUpdateConfigJSONNoWaitRefusesAHeldLock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "settings", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"cli_theme":"dark"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	unlock, err := lockConfigFile(path)
	if err != nil {
		t.Fatalf("lockConfigFile: %v", err)
	}
	write := func(cur *map[string]any, _ bool) error {
		(*cur)["cli_theme"] = "light"
		return nil
	}
	if err := UpdateConfigJSONNoWait(path, 0o600, write); !errors.Is(err, ErrConfigLocked) {
		t.Fatalf("UpdateConfigJSONNoWait with the lock held = %v, want ErrConfigLocked", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != body {
		t.Fatalf("file after the refused write = %q, %v; want it unchanged", got, err)
	}
	unlock()

	if err := UpdateConfigJSONNoWait(path, 0o600, write); err != nil {
		t.Fatalf("UpdateConfigJSONNoWait after unlock = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == body {
		t.Fatalf("file after the write = %q, want the changed document", got)
	}
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A backup is the previous copy with the file's own mode, and a path that is not
// there yet has nothing to keep.
func TestBackupFileKeepsThePreviousCopy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if got, err := BackupFile(path, "20261025T120000Z"); err != nil || got != "" {
		t.Fatalf("BackupFile of a missing path = %q, %v; want nothing to keep", got, err)
	}

	body := []byte(`{"role_agents":{"crew":"claude-sonnet"}}`)
	if err := os.WriteFile(path, body, 0o640); err != nil {
		t.Fatal(err)
	}
	dst, err := BackupFile(path, "20261025T120000Z")
	if err != nil {
		t.Fatalf("BackupFile: %v", err)
	}
	if want := path + ".bak-20261025T120000Z"; dst != want {
		t.Errorf("backup = %q, want %q", dst, want)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != string(body) {
		t.Errorf("backup content = %q, %v; want the file's", got, err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("backup mode = %v, want the file's 0640", fi.Mode().Perm())
	}
}

// Two writes inside one stamp's resolution leave two copies: the older file is
// the one an operator restores, so a backup never replaces one (gt-jaagc).
func TestBackupFileNeverOverwritesAnEarlierCopy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := BackupFile(path, "20261025T120000Z")
	if err != nil {
		t.Fatalf("BackupFile: %v", err)
	}
	if err := os.WriteFile(path, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := BackupFile(path, "20261025T120000Z")
	if err != nil {
		t.Fatalf("the second BackupFile: %v", err)
	}
	if second == first {
		t.Fatalf("the second backup was written over the first at %q", first)
	}
	if !strings.HasPrefix(second, first+"-") {
		t.Errorf("second backup = %q, want the first's name with a suffix", second)
	}
	got, err := os.ReadFile(first)
	if err != nil || string(got) != "first\n" {
		t.Errorf("the first copy = %q, %v; want the file as it was before the second write", got, err)
	}
}

// The suffix search is bounded: a directory that has collected BackupNameAttempts
// copies is reported, not searched forever (gt-jaagc).
func TestBackupFileStopsAfterBackupNameAttempts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := path + ".bak-20261025T120000Z"
	for i := 0; i < BackupNameAttempts; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i+1)
		}
		if err := os.WriteFile(name, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := BackupFile(path, "20261025T120000Z")
	if err == nil {
		t.Fatalf("BackupFile with every name taken = nil, want the taken names reported")
	}
	if !strings.Contains(err.Error(), "already exist") {
		t.Errorf("BackupFile = %v, want it to say the names are taken", err)
	}
}

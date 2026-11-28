package townflight

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestListEntries checks the listing is the .cmd files, a missing directory is empty, and an unreadable one is an error.
func TestListEntries(t *testing.T) {
	t.Parallel()
	t.Run("lists only the .cmd files", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := inflightDir(townRoot)
		if err := os.MkdirAll(filepath.Join(dir, "sub.cmd"), 0o755); err != nil {
			t.Fatalf("making subdirectory: %v", err)
		}
		for _, name := range []string{"a.cmd", "b.cmd", "notes.txt", "inflight-123"} {
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
				t.Fatalf("writing %s: %v", name, err)
			}
		}
		got, err := listEntries(townRoot)
		if err != nil {
			t.Fatalf("listEntries: %v", err)
		}
		if want := []string{filepath.Join(dir, "a.cmd"), filepath.Join(dir, "b.cmd")}; !slices.Equal(got, want) {
			t.Errorf("listEntries = %v, want %v", got, want)
		}
	})
	t.Run("missing directory is empty", func(t *testing.T) {
		t.Parallel()
		got, err := listEntries(t.TempDir())
		if err != nil {
			t.Fatalf("listEntries: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("listEntries = %v, want none", got)
		}
	})
	t.Run("unreadable directory is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		// A regular file at the in-flight path makes the read fail with ENOTDIR.
		if err := os.MkdirAll(filepath.Dir(inflightDir(townRoot)), 0o755); err != nil {
			t.Fatalf("making daemon dir: %v", err)
		}
		if err := os.WriteFile(inflightDir(townRoot), nil, 0o600); err != nil {
			t.Fatalf("writing blocker: %v", err)
		}
		if _, err := listEntries(townRoot); err == nil {
			t.Fatal("listEntries over a regular file returned no error")
		}
	})
}

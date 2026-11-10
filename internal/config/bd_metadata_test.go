package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBeadsFileDatabase: the error tells a missing file, a file that does not
// parse, and a file that names no database apart, so a caller never reads a
// broken file as absent.
func TestBeadsFileDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("missing file is fs.ErrNotExist and not present", func(t *testing.T) {
		t.Parallel()
		_, present, err := BeadsFileDatabase(filepath.Join(dir, "absent.json"))
		if !errors.Is(err, fs.ErrNotExist) || present {
			t.Errorf("present = %v, error = %v; want not present and fs.ErrNotExist", present, err)
		}
	})
	t.Run("file that does not parse is an error naming the path", func(t *testing.T) {
		t.Parallel()
		path := write("broken.json", "{not json")
		db, present, err := BeadsFileDatabase(path)
		if err == nil || errors.Is(err, fs.ErrNotExist) || db != "" {
			t.Fatalf("BeadsFileDatabase = %q, %v, %v; want an empty name and a parse error", db, present, err)
		}
		if !present {
			t.Errorf("present = false for a file that exists; want true")
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error = %v, want it to name %s", err, path)
		}
	})
	t.Run("parsed name is trimmed", func(t *testing.T) {
		t.Parallel()
		db, present, err := BeadsFileDatabase(write("named.json", `{"dolt_database":"  gt  "}`))
		if err != nil || !present || db != "gt" {
			t.Errorf("BeadsFileDatabase = %q, %v, %v; want gt, present, nil", db, present, err)
		}
	})
	t.Run("file naming no database is not an error", func(t *testing.T) {
		t.Parallel()
		db, present, err := BeadsFileDatabase(write("empty.json", `{}`))
		if err != nil || !present || db != "" {
			t.Errorf("BeadsFileDatabase = %q, %v, %v; want empty, present, nil", db, present, err)
		}
	})
}

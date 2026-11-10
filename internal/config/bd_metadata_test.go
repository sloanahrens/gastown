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

	t.Run("missing file is fs.ErrNotExist", func(t *testing.T) {
		t.Parallel()
		_, err := BeadsFileDatabase(filepath.Join(dir, "absent.json"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("error = %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("file that does not parse is an error naming the path", func(t *testing.T) {
		t.Parallel()
		path := write("broken.json", "{not json")
		db, err := BeadsFileDatabase(path)
		if err == nil || errors.Is(err, fs.ErrNotExist) || db != "" {
			t.Fatalf("BeadsFileDatabase = %q, %v; want an empty name and a parse error", db, err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error = %v, want it to name %s", err, path)
		}
	})
	t.Run("file that cannot be read is an error, not absence", func(t *testing.T) {
		t.Parallel()
		// A directory named metadata.json exists but cannot be read as a file.
		path := filepath.Join(dir, "unreadable.json")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := BeadsFileDatabase(path)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("error = %v, want a read error that is not fs.ErrNotExist", err)
		}
	})
	t.Run("parsed name is trimmed", func(t *testing.T) {
		t.Parallel()
		db, err := BeadsFileDatabase(write("named.json", `{"dolt_database":"  gt  "}`))
		if err != nil || db != "gt" {
			t.Errorf("BeadsFileDatabase = %q, %v; want gt, nil", db, err)
		}
	})
	t.Run("file naming no database is not an error", func(t *testing.T) {
		t.Parallel()
		db, err := BeadsFileDatabase(write("empty.json", `{}`))
		if err != nil || db != "" {
			t.Errorf("BeadsFileDatabase = %q, %v; want empty, nil", db, err)
		}
	})
}

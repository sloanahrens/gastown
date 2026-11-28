package townflight

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEntryLabel checks a written label is returned, and that a path which
// gives none yields text naming it.
func TestEntryLabel(t *testing.T) {
	t.Parallel()
	e, err := createEntry(t.TempDir(), "gt status")
	if err != nil {
		t.Fatalf("createEntry: %v", err)
	}
	defer e.drop() //nolint:errcheck // best-effort cleanup
	if got := entryLabel(e.path); got != "gt status" {
		t.Errorf("entryLabel = %q, want %q", got, "gt status")
	}
	empty := filepath.Join(t.TempDir(), "empty.cmd")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("writing empty file: %v", err)
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "gone.cmd"), t.TempDir(), empty} {
		if got := entryLabel(path); !strings.Contains(got, "could not be read") || !strings.Contains(got, path) {
			t.Errorf("entryLabel(%q) = %q, want the could-not-be-read text naming the path", path, got)
		}
	}
	partial := func(string) ([]byte, error) { return []byte("gt sta"), errors.New("i/o error") }
	if got := entryLabelWith(partial, e.path); strings.Contains(got, "gt sta") || !strings.Contains(got, "could not be read") {
		t.Errorf("entryLabelWith after a failed read = %q, want the could-not-be-read text", got)
	}
}

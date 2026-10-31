package ring

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestPresent: a town with no ring section reports false, a town that holds one
// reports true whatever it holds, and a town config that cannot be read is an
// error, never "no Ring".
func TestPresent(t *testing.T) {
	t.Parallel()
	t.Run("no mayor/town.json", func(t *testing.T) {
		t.Parallel()
		got, err := Present(t.TempDir())
		if err != nil || got {
			t.Fatalf("Present with no town config = %v, %v; want false, nil", got, err)
		}
	})
	t.Run("a town with no ring section", func(t *testing.T) {
		t.Parallel()
		root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
		got, err := Present(root)
		if err != nil || got {
			t.Fatalf("Present with no ring section = %v, %v; want false, nil", got, err)
		}
	})
	t.Run("a ring section, whatever it holds", func(t *testing.T) {
		t.Parallel()
		root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
		writeSection(t, root, "not a record")
		got, err := Present(root)
		if err != nil || !got {
			t.Fatalf("Present with a ring section = %v, %v; want true, nil", got, err)
		}
	})
	t.Run("an unreadable town config is an error", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		path := mustTownPath(t, root)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := Present(root)
		if err == nil {
			t.Fatalf("Present with an unparseable town config = %v, nil; want an error", got)
		}
		if got {
			t.Error("Present reported a ring section it could not read")
		}
		if errors.Is(err, config.ErrNotFound) {
			t.Errorf("Present reported the unreadable config as missing: %v", err)
		}
	})
}

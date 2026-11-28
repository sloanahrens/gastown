package townflight

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func heldEntry(t *testing.T, townRoot, label string) {
	t.Helper()
	e, err := createEntry(inflightDir(townRoot), label)
	if err != nil {
		t.Fatalf("createEntry: %v", err)
	}
	held := e.path + ".cmd"
	if err := os.Rename(e.path, held); err != nil {
		t.Fatalf("listing entry: %v", err)
	}
	e.path = held
	t.Cleanup(func() { _ = e.drop() })
}

// TestPending checks Pending lists held entries, drops an ended one, and
// returns a listing or probe error rather than an empty list.
func TestPending(t *testing.T) {
	t.Parallel()
	t.Run("lists held entries and drops the ended one", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		heldEntry(t, townRoot, "gt sling")
		heldEntry(t, townRoot, "gt status")
		ended := filepath.Join(inflightDir(townRoot), "inflight-ended.cmd")
		if err := os.WriteFile(ended, nil, 0o600); err != nil {
			t.Fatalf("writing ended entry: %v", err)
		}
		got, err := Pending(townRoot)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		slices.Sort(got)
		if want := []string{"gt sling", "gt status"}; !slices.Equal(got, want) {
			t.Errorf("Pending = %v, want %v", got, want)
		}
		if _, err := os.Stat(ended); !os.IsNotExist(err) {
			t.Errorf("Stat of the ended entry = %v, want not exist", err)
		}
	})
	t.Run("listing error is returned", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(inflightDir(townRoot)), 0o755); err != nil {
			t.Fatalf("making daemon directory: %v", err)
		}
		// A regular file at the in-flight path makes the listing fail.
		if err := os.WriteFile(inflightDir(townRoot), nil, 0o600); err != nil {
			t.Fatalf("writing blocker: %v", err)
		}
		if got, err := Pending(townRoot); err == nil || got != nil {
			t.Errorf("Pending = %v, %v; want nil and a listing error", got, err)
		}
	})
	t.Run("probe error is returned", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := inflightDir(townRoot)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("making in-flight directory: %v", err)
		}
		// An entry whose target's directory is missing lists, then fails to lock.
		if err := os.Symlink(filepath.Join(dir, "missing", "entry"), filepath.Join(dir, "broken.cmd")); err != nil {
			t.Fatalf("making unopenable entry: %v", err)
		}
		if got, err := Pending(townRoot); err == nil || got != nil {
			t.Errorf("Pending = %v, %v; want nil and a probe error", got, err)
		}
	})
}

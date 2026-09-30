package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/wisp"
)

// TestAreScheduledFailClosed: when no town can be found from the cwd, every
// requested bead reads as scheduled (fail closed), and no sling context is
// read at all. This prevents false stranded detection and duplicate
// scheduling on transient errors.
func TestAreScheduledFailClosed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		root string
		err  error
	}{
		{"no town", "", nil},
		{"unreadable cwd", "", errors.New("getwd: permission denied")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			requestedIDs := []string{"bead-1", "bead-2", "bead-3"}
			result := areScheduledWith("", requestedIDs,
				func() (string, error) { return tt.root, tt.err },
				func(string, []string) map[string]bool {
					t.Fatal("areScheduled read sling contexts without a town")
					return nil
				})

			for _, id := range requestedIDs {
				if !result[id] {
					t.Errorf("areScheduled fail-closed: expected %q to be marked as scheduled, but it was not", id)
				}
			}
		})
	}
}

// TestShouldDeferDispatchNoTownIsDirect pins the one case where an absent
// answer legitimately means "direct dispatch": the cwd is readable and simply
// not inside a town. It must not fall through to LoadOrCreateTownSettings
// with an empty root, which would write settings/config.json into the cwd
// (gt-udrrw, gt-bfale).
func TestShouldDeferDispatchNoTownIsDirect(t *testing.T) {
	t.Parallel()
	deferred, err := shouldDeferDispatchWith(
		func() (string, error) { return "", nil },
		func(path string) (*config.TownSettings, error) {
			t.Fatalf("shouldDeferDispatch loaded (and could create) settings at %q outside a town", path)
			return nil, nil
		})
	if err != nil {
		t.Fatalf("shouldDeferDispatch outside a town: unexpected error %v", err)
	}
	if deferred {
		t.Error("shouldDeferDispatch outside a town = true, want false (direct dispatch)")
	}
}

// TestAreScheduledEmptyInput verifies areScheduled returns empty map for no input.
func TestAreScheduledEmptyInput(t *testing.T) {
	t.Parallel()
	result := areScheduled(nil)
	if len(result) != 0 {
		t.Errorf("areScheduled(nil) should return empty map, got %d entries", len(result))
	}
	result = areScheduled([]string{})
	if len(result) != 0 {
		t.Errorf("areScheduled([]) should return empty map, got %d entries", len(result))
	}
}

// TestResolveFormula verifies formula resolution precedence:
// explicit flag > wisp layer > bead layer > system default > settings file > hardcoded fallback.
func TestResolveFormula(t *testing.T) {
	t.Parallel()

	t.Run("explicit flag wins", func(t *testing.T) {
		t.Parallel()
		got := resolveFormula("mol-evolve", false, "/tmp/nonexistent", "myrig")
		if got != "mol-evolve" {
			t.Errorf("got %q, want %q", got, "mol-evolve")
		}
	})

	t.Run("hookRawBead returns empty", func(t *testing.T) {
		t.Parallel()
		got := resolveFormula("mol-evolve", true, "/tmp/nonexistent", "myrig")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("system default mol-polecat-work", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		rigName := "testrig"
		_ = os.MkdirAll(filepath.Join(tmpDir, rigName), 0o755)
		got := resolveFormula("", false, tmpDir, rigName)
		if got != "mol-polecat-work" {
			t.Errorf("got %q, want %q", got, "mol-polecat-work")
		}
	})

	t.Run("wisp layer overrides system default", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		rigName := "testrig"
		_ = os.MkdirAll(filepath.Join(tmpDir, rigName), 0o755)

		wispCfg := wisp.NewConfig(tmpDir, rigName)
		if err := wispCfg.Set("default_formula", "mol-evolve"); err != nil {
			t.Fatalf("wisp set: %v", err)
		}

		got := resolveFormula("", false, tmpDir, rigName)
		if got != "mol-evolve" {
			t.Errorf("got %q, want %q", got, "mol-evolve")
		}
	})

	t.Run("explicit flag overrides wisp layer", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		rigName := "testrig"
		_ = os.MkdirAll(filepath.Join(tmpDir, rigName), 0o755)

		wispCfg := wisp.NewConfig(tmpDir, rigName)
		if err := wispCfg.Set("default_formula", "mol-evolve"); err != nil {
			t.Fatalf("wisp set: %v", err)
		}

		got := resolveFormula("mol-custom", false, tmpDir, rigName)
		if got != "mol-custom" {
			t.Errorf("got %q, want %q", got, "mol-custom")
		}
	})

	t.Run("empty rigName falls back to hardcoded default", func(t *testing.T) {
		t.Parallel()
		got := resolveFormula("", false, "/tmp/nonexistent", "")
		if got != "mol-polecat-work" {
			t.Errorf("got %q, want %q", got, "mol-polecat-work")
		}
	})
}

package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFindOrCreateTown(t *testing.T) {
	t.Parallel()
	newTown := func(t *testing.T) string {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "mayor"), 0755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	env := func(root string) func(string) string {
		return func(k string) string {
			if k == "GT_TOWN_ROOT" {
				return root
			}
			return ""
		}
	}
	noCwdTown := func() (string, error) { return "", errors.New("not in a town") }

	t.Run("GT_TOWN_ROOT takes priority over the cwd town", func(t *testing.T) {
		t.Parallel()
		town := newTown(t)
		cwdTown := func() (string, error) { return "/cwd/town", nil }
		got, err := findOrCreateTown(env(town), cwdTown, func() (string, error) { return t.TempDir(), nil })
		if err != nil || got != town {
			t.Fatalf("findOrCreateTown() = %q, %v; want %q", got, err, town)
		}
	})

	t.Run("invalid GT_TOWN_ROOT falls back to the cwd town", func(t *testing.T) {
		t.Parallel()
		cwdTown := func() (string, error) { return "/cwd/town", nil }
		got, err := findOrCreateTown(env("/nonexistent/path/to/town"), cwdTown, func() (string, error) { return t.TempDir(), nil })
		if err != nil || got != "/cwd/town" {
			t.Fatalf("findOrCreateTown() = %q, %v; want /cwd/town", got, err)
		}
	})

	t.Run("falls back to ~/gt", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, "gt", "mayor"), 0755); err != nil {
			t.Fatal(err)
		}
		got, err := findOrCreateTown(env(""), noCwdTown, func() (string, error) { return home, nil })
		if want := filepath.Join(home, "gt"); err != nil || got != want {
			t.Fatalf("findOrCreateTown() = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("no town anywhere is an error", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		if _, err := findOrCreateTown(env(""), noCwdTown, func() (string, error) { return home, nil }); err == nil {
			t.Fatal("want an error when no town exists")
		}
	})
}

func TestIsValidTown(t *testing.T) {
	t.Parallel()
	t.Run("valid town has mayor directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		mayorDir := filepath.Join(tmpDir, "mayor")
		if err := os.MkdirAll(mayorDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		if !isValidTown(tmpDir) {
			t.Error("isValidTown() = false, want true")
		}
	})

	t.Run("invalid town missing mayor directory", func(t *testing.T) {
		tmpDir := t.TempDir()

		if isValidTown(tmpDir) {
			t.Error("isValidTown() = true, want false")
		}
	})

	t.Run("nonexistent path is invalid", func(t *testing.T) {
		if isValidTown("/nonexistent/path") {
			t.Error("isValidTown() = true, want false")
		}
	})
}

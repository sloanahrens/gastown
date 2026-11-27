package modroot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

func TestRootFromFindsGoModAbove(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	start := filepath.Join(root, "nested")
	if err := os.MkdirAll(filepath.Join(start, "go.mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := rootFrom(start)
	if err != nil || got != root {
		t.Fatalf("rootFrom(%s) = %s, %v; want %s", start, got, err, root)
	}
}

func TestRootFromErrorsWithNoGoMod(t *testing.T) {
	t.Parallel()
	start := t.TempDir()
	if _, err := rootFrom(start); err == nil || errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), start) {
		t.Fatalf("rootFrom(%s) = _, %v; want an error naming the start", start, err)
	}
}

func TestRootFromReturnsStatErrors(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var pe *os.PathError
	if _, err := rootFrom(filepath.Join(file, "nested")); !errors.As(err, &pe) {
		t.Fatalf("rootFrom = %v, want the stat error from the unreadable start", err)
	}
}

func TestRootFindsThisModule(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(filepath.Join(Root(t), "go.mod")); err != nil {
		t.Fatalf("Root(t) has no go.mod: %v", err)
	}
}

func TestRootResolvesSymlink(t *testing.T) {
	t.Parallel()
	physical := t.TempDir()
	if err := os.WriteFile(filepath.Join(physical, "go.mod"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "seat")
	if err := os.Symlink(physical, link); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(physical)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rootFromWD(link)
	if err != nil {
		t.Fatalf("rootFromWD(%s) = _, %v; want the physical root %s", link, err, want)
	}
	if got != want {
		t.Fatalf("rootFromWD(%s) = %s; want the physical root %s", link, got, want)
	}
}

func TestRootUnresolved(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "gone")
	got, err := rootFromWD(missing)
	if err == nil {
		t.Fatalf("rootFromWD(%s) = %s, nil; want an error naming the working directory", missing, got)
	}
	if got != "" {
		t.Fatalf("rootFromWD(%s) = %q, %v; want no root", missing, got, err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("rootFromWD(%s) error %q does not name the working directory", missing, err)
	}
}

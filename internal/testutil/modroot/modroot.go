// Package modroot finds the module root from the working directory, so a test
// reading the checkout works under go test -trimpath (gt-sb5rhu).
package modroot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Root returns the module root of the test's working directory, or fails t.
func Root(t testing.TB) string {
	t.Helper()
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("modroot: %v", err)
	}
	root, err := rootFromWD(wd)
	if err != nil {
		t.Fatalf("modroot: %v", err)
	}
	return root
}

// rootFromWD resolves the symlinks in the working directory wd and returns the
// module root above the physical path, or an error naming wd.
func rootFromWD(wd string) (string, error) {
	physical, err := filepath.EvalSymlinks(wd)
	if err != nil {
		return "", fmt.Errorf("modroot: resolving working directory %s: %w", wd, err)
	}
	return rootFrom(physical)
}

// rootFrom returns the first directory at or above the absolute start holding a go.mod file, or an error naming start.
func rootFrom(start string) (string, error) {
	dir := start
	for {
		if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !fi.IsDir() {
			return dir, nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("modroot: no go.mod at or above " + start)
		}
		dir = parent
	}
}

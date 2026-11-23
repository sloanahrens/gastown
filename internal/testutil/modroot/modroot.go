// Package modroot finds the module root from the working directory, so a test
// reading the checkout works under go test -trimpath (gt-sb5rhu).
package modroot

import (
	"errors"
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
	root, err := rootFrom(wd)
	if err != nil {
		t.Fatalf("modroot: %v", err)
	}
	return root
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

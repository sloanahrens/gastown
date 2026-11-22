// Package modroot finds the module root from the working directory, so tests
// that read the checkout work under go test -trimpath, where runtime.Caller
// reports a module path instead of a disk path (gt-sb5rhu).
package modroot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Root returns the module root of the test's working directory, failing t when
// there is none.
func Root(t testing.TB) string {
	t.Helper()
	root, err := rootFrom(".")
	if err != nil {
		t.Fatalf("modroot: %v", err)
	}
	return root
}

// rootFrom returns the first directory at or above start that holds a go.mod
// file, or an error naming start when there is none.
func rootFrom(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
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

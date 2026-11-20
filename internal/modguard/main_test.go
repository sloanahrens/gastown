// Package modguard holds the tests that scan the module's source.
package modguard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/gosrc"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// tree is the module's source, read and parsed once for every test in the
// package (gt-lckrgk).
var tree = gosrc.For(moduleRoot())

// moduleRoot is the directory holding go.mod, above the test's working directory.
func moduleRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		panic("modguard: no working directory: " + err.Error())
	}
	for ; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		} else if !os.IsNotExist(err) {
			panic("modguard: reading " + filepath.Join(dir, "go.mod") + ": " + err.Error())
		}
		if filepath.Dir(dir) == dir {
			panic("modguard: no go.mod at or above " + dir)
		}
	}
}

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

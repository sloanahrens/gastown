package beads

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// beadsModulePath is the in-process beads library gastown no longer links.
// gt-7iwy0.2 moved the last callers onto bd, and gt-7iwy0.3 deleted what was
// left of the library: the store files, the *Beads store branches, testutil's
// pooled test store, and the go.mod requirement. This test is the guard that
// keeps one from growing back, now over the whole module rather than the
// handful of files gt-7iwy0.2 had migrated.
//
// It is assembled rather than written out so that this file is not itself the
// one remaining place the module path appears in a Go string: an import is
// what the guard forbids, and grepping the tree for the quoted module path is
// how the deletion is verified.
const beadsModulePath = beadsModuleRepo + "/" + beadsModuleName

const (
	beadsModuleRepo = "github.com/steveyegge"
	beadsModuleName = "beads"
)

// TestNoGoFileImportsBeadsLibrary walks the module and fails on every import
// of the beads library, so `go version -m ./gt` stays free of it and `go mod
// tidy` never re-adds the requirement.
func TestNoGoFileImportsBeadsLibrary(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && walkSkipsDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, imp := range f.Imports {
			imported, _ := strconv.Unquote(imp.Path.Value)
			if imported == beadsModulePath || strings.HasPrefix(imported, beadsModulePath+"/") {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					rel = path
				}
				offenders = append(offenders, rel+" imports "+imported)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("gastown reads and writes beads through bd, never through the in-process library (D1, gt-7iwy0.3):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// walkSkipsDir reports whether a directory is outside the compiled module:
// the go tool ignores testdata, vendor, and directories beginning with "." or
// "_", so neither a stray import nor a fixture there reaches a build.
func walkSkipsDir(name string) bool {
	if name == "testdata" || name == "vendor" || name == "node_modules" {
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// moduleRoot is the directory holding this module's go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

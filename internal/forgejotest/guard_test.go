package forgejotest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestForgejotestOnlyImportedByTests walks the module and fails on any non-test Go file
// that imports this package, so the fake cannot reach production code. The walk must see
// the production files it is checking, or a walk that found nothing would pass.
func TestForgejotestOnlyImportedByTests(t *testing.T) {
	t.Parallel()
	const self = "github.com/steveyegge/gastown/internal/forgejotest"
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var checked int
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == self {
				offenders = append(offenders, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	if checked == 0 {
		t.Fatalf("the walk found no production files to check")
	}
	if len(offenders) != 0 {
		t.Errorf("non-test files import %s: %v", self, offenders)
	}
}

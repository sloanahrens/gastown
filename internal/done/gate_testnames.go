package done

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"unicode"
	"unicode/utf8"
)

// testFuncNames returns the test, benchmark, fuzz and example function names
// the files declare, in file then declaration order. It parses and runs
// nothing: go test -list runs the package's TestMain, which may need Docker (gt-rw4926).
func testFuncNames(fsys fs.FS, files []string) ([]string, error) {
	var names []string
	for _, file := range files {
		src, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", file, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !isTestFuncName(fn.Name.Name) {
				continue
			}
			names = append(names, fn.Name.Name)
		}
	}
	return names, nil
}

// isTestFuncName reports whether go test lists name: a prefix followed by a
// lower-case letter is not a test (Testfoo), and TestMain is never one.
func isTestFuncName(name string) bool {
	if name == "TestMain" {
		return false
	}
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		if rest == "" {
			return true
		}
		r, _ := utf8.DecodeRuneInString(rest)
		return !unicode.IsLower(r)
	}
	return false
}

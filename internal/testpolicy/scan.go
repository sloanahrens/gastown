// Package testpolicy enforces the unit-test rules described in docs/testing.md.
// It works on syntax only (go/parser), so scanning the whole repository takes
// seconds and needs no type-checking.
package testpolicy

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Violation is one broken rule at one source position.
type Violation struct {
	Pos  token.Position
	Rule string
	Msg  string
}

func (v Violation) String() string { return fmt.Sprintf("%s: [%s] %s", v.Pos, v.Rule, v.Msg) }

// ScanDir checks the Go files of one package directory, not recursively.
// Files excluded by build constraints for the current platform with no extra
// tags are skipped, and that includes every //go:build integration file.
func ScanDir(dir string) ([]Violation, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var prod, tests []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		ok, err := build.Default.MatchFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, name), err)
		}
		if !ok {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(name, "_test.go") {
			tests = append(tests, f)
		} else {
			prod = append(prod, f)
		}
	}
	pkgVars := packageVars(prod)
	clocked := importsPath(prod, "github.com/jonboulle/clockwork")
	var vs []Violation
	for _, f := range tests {
		vs = append(vs, checkTestFile(fset, f, pkgVars)...)
	}
	for _, f := range prod {
		vs = append(vs, checkProdFile(fset, f, clocked)...)
	}
	vs = applyAllows(fset, append(append([]*ast.File{}, tests...), prod...), vs)
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].Pos.Filename != vs[j].Pos.Filename {
			return vs[i].Pos.Filename < vs[j].Pos.Filename
		}
		return vs[i].Pos.Line < vs[j].Pos.Line
	})
	return vs, nil
}

// packageVars returns the names of package-level variables declared in the
// production files of a package.
func packageVars(files []*ast.File) map[string]bool {
	vars := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, s := range gd.Specs {
				for _, n := range s.(*ast.ValueSpec).Names {
					vars[n.Name] = true
				}
			}
		}
	}
	return vars
}

func importsPath(files []*ast.File, path string) bool {
	for _, f := range files {
		for _, im := range f.Imports {
			if strings.Trim(im.Path.Value, `"`) == path {
				return true
			}
		}
	}
	return false
}

// imports maps each import's local name to its path for one file.
func imports(f *ast.File) map[string]string {
	m := map[string]string{}
	for _, im := range f.Imports {
		p := strings.Trim(im.Path.Value, `"`)
		name := filepath.Base(p)
		if im.Name != nil {
			name = im.Name.Name
		}
		m[name] = p
	}
	return m
}

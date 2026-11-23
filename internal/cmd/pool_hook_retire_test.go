package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// poolPackageSources names the package's non-test Go files, in the directory
// `go test` runs each test from.
func poolPackageSources(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, filepath.Join(".", name))
	}
	if len(names) == 0 {
		t.Fatal("found no package sources")
	}
	return names
}

// receiverName names the receiver's base type, unwrapping the pointer.
func receiverName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// TestPoolHookRetirePlumbing is gt-hz9wkp: the hook read's plumbing is gone as
// written. poolRouter carries no hookHeld field and poolSeatBatch no hookHeld
// method. The field is checked by reflection; the method is unexported, which
// reflection does not report on a concrete type, so it is checked in the
// declarations.
func TestPoolHookRetirePlumbing(t *testing.T) {
	t.Parallel()
	if _, ok := reflect.TypeOf(poolRouter{}).FieldByName("hookHeld"); ok {
		t.Error("poolRouter still has a hookHeld field")
	}

	fset := token.NewFileSet()
	for _, path := range poolPackageSources(t) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "hookHeld" {
				continue
			}
			for _, field := range fn.Recv.List {
				if receiverName(field.Type) == "poolSeatBatch" {
					t.Errorf("%s: poolSeatBatch still has a hookHeld method", path)
				}
			}
		}
	}
}

// TestPoolHookRetirePlumbingSources is the git-grep half of gt-hz9wkp: no
// non-test source names the removed seam, its router field or its batch
// method.
func TestPoolHookRetirePlumbingSources(t *testing.T) {
	t.Parallel()
	forbidden := []string{"poolHookHeldFor", "batch.hookHeld", "r.hookHeld"}
	for _, path := range poolPackageSources(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, symbol := range forbidden {
			if strings.Contains(string(src), symbol) {
				t.Errorf("%s still mentions %s", path, symbol)
			}
		}
	}
}

// TestPoolHookRetireSignature is gt-ujao98, the last of the hook read's
// retirement: the three outer pool functions no longer take the hook-held seam
// the earlier beads emptied, and the type they named is gone. It is red while
// any signature still carries a polecatHookHeldFunc parameter.
func TestPoolHookRetireSignature(t *testing.T) {
	t.Parallel()
	outer := map[string]any{
		"poolSeatSessionsWith":       poolSeatSessionsWith,
		"poolSeatSessionsProbedWith": poolSeatSessionsProbedWith,
		"poolSeatSnapshotWith":       poolSeatSnapshotWith,
	}
	for name, fn := range outer {
		typ := reflect.TypeOf(fn)
		for i := 0; i < typ.NumIn(); i++ {
			if got := typ.In(i).Name(); got == "polecatHookHeldFunc" {
				t.Errorf("%s still takes a %s parameter", name, got)
			}
		}
	}

	fset := token.NewFileSet()
	for _, path := range poolPackageSources(t) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == "polecatHookHeldFunc" {
					t.Errorf("%s still declares the polecatHookHeldFunc type", path)
				}
			}
		}
	}
}

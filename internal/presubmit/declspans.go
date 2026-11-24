package presubmit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
)

// declsTouching returns the sorted, de-duplicated names of the top-level
// declarations in src whose span holds one of lines (gt-roeu23).
//
// A function, method, type, var or const is named by its identifier; a method
// is named by its method name, not its receiver. The span runs from a
// declaration's own first token to its last, so a line inside its doc comment
// names nothing. Each spec of a grouped var, const or type block is its own
// span, so a line names only the spec it is in. src that does not parse is an
// error; no lines, or lines outside every declaration, name nothing and are no
// error.
func declsTouching(src []byte, lines []int) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse source: %w", err)
	}
	names := map[string]bool{}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if touches(fset, d, lines) {
				names[d.Name.Name] = true
			}
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			for _, spec := range d.Specs {
				if !touches(fset, spec, lines) {
					continue
				}
				for _, name := range specNames(spec) {
					names[name] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(names)), nil
}

// specNames returns the names one var, const or type spec declares.
func specNames(spec ast.Spec) []string {
	switch s := spec.(type) {
	case *ast.ValueSpec:
		names := make([]string, 0, len(s.Names))
		for _, id := range s.Names {
			names = append(names, id.Name)
		}
		return names
	case *ast.TypeSpec:
		return []string{s.Name.Name}
	}
	return nil
}

// touches reports whether one of lines falls inside a node's span, read from
// the file set the node was parsed with.
func touches(fset *token.FileSet, node ast.Node, lines []int) bool {
	first, last := fset.Position(node.Pos()).Line, fset.Position(node.End()).Line
	for _, line := range lines {
		if first <= line && line <= last {
			return true
		}
	}
	return false
}

package presubmit

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ChangedTests names, sorted, the top-level Test functions a test file gained or whose body changed between its content at the merge base and at HEAD, so the gate can require them to fail without the change (gt-13dre6). A nil base means the file is absent at the merge base, so every test in head is new. Comment and layout edits are no body change, because bodies compare as gofmt prints them. An integration-tagged head file gives no names, since the gate runs no integration test. A file that does not parse at either side is an error naming it.
func ChangedTests(path string, base, head []byte) ([]string, error) {
	return changedTests(path, base, head, func(w io.Writer, body *ast.BlockStmt) error {
		return format.Node(w, token.NewFileSet(), body)
	})
}

// changedTests is ChangedTests with its body printer injected, so a test can drive the print failure.
func changedTests(path string, base, head []byte, print func(io.Writer, *ast.BlockStmt) error) ([]string, error) {
	if IntegrationTagged(head) {
		return nil, nil
	}
	headTests, err := testBodies(path, head, print)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return slices.Sorted(maps.Keys(headTests)), nil
	}
	baseTests, err := testBodies(path, base, print)
	if err != nil {
		return nil, err
	}
	var changed []string
	for name, body := range headTests {
		if old, ok := baseTests[name]; !ok || old != body {
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	return changed, nil
}

// testBodies parses one test file and maps each test's name to its body as gofmt prints it, so a comment or layout edit does not read as a body change.
func testBodies(path string, src []byte, print func(io.Writer, *ast.BlockStmt) error) (map[string]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || !isTestFunc(fd) {
			continue
		}
		var buf strings.Builder
		if err := print(&buf, fd.Body); err != nil {
			return nil, fmt.Errorf("%s: printing %s: %w", path, fd.Name.Name, err)
		}
		out[fd.Name.Name] = buf.String()
	}
	return out, nil
}

// isTestFunc reports whether fd is a test: a top-level func named Test, or Test followed by a non-lowercase letter, other than TestMain, taking one *testing.T. Methods, benchmarks, fuzz targets and helpers are not.
func isTestFunc(fd *ast.FuncDecl) bool {
	rest, prefixed := strings.CutPrefix(fd.Name.Name, "Test")
	if !prefixed || fd.Recv != nil || fd.Body == nil || fd.Name.Name == "TestMain" {
		return false
	}
	if r, _ := utf8.DecodeRuneInString(rest); rest != "" && unicode.IsLower(r) {
		return false
	}
	params := fd.Type.Params.List
	return len(params) == 1 && len(params[0].Names) <= 1 && isTestingTPointer(params[0].Type)
}

// isTestingTPointer reports whether t is the type *testing.T.
func isTestingTPointer(t ast.Expr) bool {
	star, ok := t.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "testing" && sel.Sel.Name == "T"
}

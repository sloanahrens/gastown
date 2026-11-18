package testpolicy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
)

// RuleEnvArgs is a test failure message that prints an environment.
const RuleEnvArgs = "env-args"

// envPrintMarker exempts the one call it trails on that call's line.
const envPrintMarker = "testpolicy:env-print-ok"

// failureMethods are the calls whose arguments reach a test's output.
var failureMethods = map[string]bool{"Fatal": true, "Fatalf": true, "Error": true,
	"Errorf": true, "Log": true, "Logf": true}

// envArgNames are the names a test conventionally binds an environment to.
var envArgNames = map[string]bool{"env": true, "environ": true, "cmdEnv": true, "gotEnv": true}

// ScanEnvArgs parses every *_test.go under root/internal and root/cmd, build tags
// included, and reports failure args that print an environment; an unreadable file errors.
func ScanEnvArgs(root string) ([]Violation, error) {
	fset, vs := token.NewFileSet(), []Violation{}
	for _, sub := range []string{"cmd", "internal"} {
		if err := filepath.WalkDir(filepath.Join(root, sub), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return err
			}
			vs = append(vs, envArgsFile(fset, f)...)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return vs, nil
}

// envArgsFile reports one test file's failure calls; a marker exempts the nearest
// call it trails on the same line.
func envArgsFile(fset *token.FileSet, f *ast.File) []Violation {
	var calls []*ast.CallExpr
	byCall := map[*ast.CallExpr][]Violation{}
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok || !failureMethods[sel.Sel.Name] {
			return true
		}
		calls = append(calls, c)
		for _, arg := range c.Args {
			if pos, bad := envArg(arg, imports(f)); bad {
				byCall[c] = append(byCall[c], Violation{Pos: fset.Position(pos), Rule: RuleEnvArgs,
					Msg: sel.Sel.Name + " prints an environment; name the key or its entry count"})
			}
		}
		return true
	})
	for _, g := range f.Comments {
		for _, c := range g.List {
			if !strings.Contains(c.Text, envPrintMarker) {
				continue
			}
			for pos, i := fset.Position(c.Slash), len(calls)-1; i >= 0; i-- {
				if end := fset.Position(calls[i].End()); end.Line == pos.Line && end.Offset <= pos.Offset {
					delete(byCall, calls[i])
					break
				}
			}
		}
	}
	var vs []Violation
	for _, c := range calls {
		vs = append(vs, byCall[c]...)
	}
	return vs
}

// envArg reports whether e names an environment directly: os.Environ(), a plain
// identifier named for one, or a fmt.Sprintf carrying one. Nothing else is.
func envArg(e ast.Expr, imp map[string]string) (token.Pos, bool) {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Pos(), envArgNames[e.Name]
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return token.NoPos, false
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok || x.Obj != nil {
			return token.NoPos, false
		}
		switch imp[x.Name] + "." + sel.Sel.Name {
		case "os.Environ":
			return e.Pos(), true
		case "fmt.Sprintf":
			for _, arg := range e.Args {
				if pos, bad := envArg(arg, imp); bad {
					return pos, true
				}
			}
		}
	}
	return token.NoPos, false
}

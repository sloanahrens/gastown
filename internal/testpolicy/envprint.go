package testpolicy

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
)

// RuleEnvPrint is a test failure message that prints the environment.
const RuleEnvPrint = "env-print"

// envPrintNames are the identifiers a failure message must not print. A
// failing test run in a sandbox seat reports its message into the agent's
// conversation, and a credential placeholder there ends the seat, so the
// message names the key or a count and never the slice.
var envPrintNames = map[string]bool{
	"env":     true,
	"environ": true,
	"cmdEnv":  true,
	"gotEnv":  true,
}

const envPrintMarker = "testpolicy:env-print-ok"

// CheckEnvPrints walks root recursively and reports every Fatalf, Errorf or
// Logf call whose argument list prints the environment: an argument that is an
// identifier named env, environ, cmdEnv or gotEnv, a call to os.Environ, or a
// selector ending in .Env.
//
// An argument is judged as a whole expression. A count or length wrapped
// around the value, such as len(env), prints no variable and passes.
//
// A call carrying a "// testpolicy:env-print-ok <reason>" comment on the same
// line or the line above is skipped: it prints a value that is provably a host
// and a port, which the reason names.
func CheckEnvPrints(root string) ([]Violation, error) {
	fset := token.NewFileSet()
	var vs []Violation
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		exempt := envPrintExemptLines(fset, f)
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Fatalf", "Errorf", "Logf":
			default:
				return true
			}
			pos := fset.Position(c.Pos())
			if exempt[pos.Line] {
				return true
			}
			for i, arg := range c.Args {
				if !printsEnvPrintArg(arg) {
					continue
				}
				vs = append(vs, Violation{
					Pos:  pos,
					Rule: RuleEnvPrint,
					Msg:  fmt.Sprintf("%s prints the environment in argument %d; name the key or a count instead, or declare the value safe with %q", sel.Sel.Name, i+1, "// "+envPrintMarker+" <reason>"),
				})
				break
			}
			return true
		})
		return nil
	})
	return vs, err
}

// envPrintExemptLines returns the call line and the line above it for each
// env-print-ok comment, so the comment may sit on the call or on its own line.
func envPrintExemptLines(fset *token.FileSet, f *ast.File) map[int]bool {
	lines := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			if !strings.HasPrefix(text, envPrintMarker) {
				continue
			}
			line := fset.Position(c.Pos()).Line
			lines[line] = true
			lines[line+1] = true
		}
	}
	return lines
}

// printsEnvPrintArg reports whether one argument of a failure call prints the
// environment.
func printsEnvPrintArg(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return envPrintNames[e.Name]
	case *ast.SelectorExpr:
		return e.Sel.Name == "Env"
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		// x.Obj != nil means a local value shadows the package name, so this
		// is not os.Environ.
		return ok && x.Obj == nil && x.Name == "os" && sel.Sel.Name == "Environ"
	}
	return false
}

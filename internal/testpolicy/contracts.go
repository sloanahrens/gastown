package testpolicy

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	RuleContract        = "fake-contract"
	RuleIntegrationName = "integration-name"
)

var contractName = regexp.MustCompile(`^Run[A-Z]\w*Contract$`)

// CheckContracts enforces that every *fake package exports a Run…Contract
// function, that some integration-tagged file calls it, and that integration
// tests are named TestIntegration….
func CheckContracts(root string, dirs []string) ([]Violation, error) {
	fset := token.NewFileSet()
	want := map[string]token.Position{} // "fakepkg.RunXContract" -> declaration
	called := map[string]bool{}
	var vs []Violation
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		isFake := strings.HasSuffix(filepath.Base(dir), "fake")
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") {
				continue
			}
			path := filepath.Join(dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return nil, err
			}
			integration := hasIntegrationTag(f)
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv != nil {
					continue
				}
				if isFake && !strings.HasSuffix(name, "_test.go") && contractName.MatchString(fd.Name.Name) {
					want[filepath.Base(dir)+"."+fd.Name.Name] = fset.Position(fd.Pos())
				}
				if integration && strings.HasPrefix(fd.Name.Name, "Test") && !strings.HasPrefix(fd.Name.Name, "TestIntegration") && fd.Name.Name != "TestMain" {
					vs = append(vs, Violation{fset.Position(fd.Pos()), RuleIntegrationName, fd.Name.Name + " is in an integration file; name it TestIntegration…"})
				}
			}
			if integration {
				ast.Inspect(f, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok && contractName.MatchString(sel.Sel.Name) {
						if x, ok := sel.X.(*ast.Ident); ok {
							called[x.Name+"."+sel.Sel.Name] = true
						}
					}
					return true
				})
			}
		}
		if isFake {
			found := false
			for k := range want {
				if strings.HasPrefix(k, filepath.Base(dir)+".") {
					found = true
				}
			}
			if !found {
				vs = append(vs, Violation{token.Position{Filename: dir}, RuleContract, "fake package exports no Run…Contract function"})
			}
		}
	}
	for k, pos := range want {
		if !called[k] {
			vs = append(vs, Violation{pos, RuleContract, k + " is never run against the real implementation in an integration test"})
		}
	}
	return vs, nil
}

func hasIntegrationTag(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) {
				expr, err := constraint.Parse(c.Text)
				if err == nil && mentionsIntegration(expr) {
					return true
				}
			}
		}
	}
	return false
}

// mentionsIntegration reports whether expr refers to the "integration" build
// tag anywhere in it. Evaluating the expression with a witness function
// (ok("integration") == true, everything else false) is not enough: a
// platform constraint like "!windows" also evaluates true under that witness
// even though it has nothing to do with integration tests, so the tag must
// be found syntactically instead.
func mentionsIntegration(e constraint.Expr) bool {
	switch e := e.(type) {
	case *constraint.TagExpr:
		return e.Tag == "integration"
	case *constraint.NotExpr:
		return mentionsIntegration(e.X)
	case *constraint.AndExpr:
		return mentionsIntegration(e.X) || mentionsIntegration(e.Y)
	case *constraint.OrExpr:
		return mentionsIntegration(e.X) || mentionsIntegration(e.Y)
	}
	return false
}

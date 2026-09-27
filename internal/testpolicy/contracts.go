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
				if err == nil && requiresIntegration(expr) {
					return true
				}
			}
		}
	}
	return false
}

// requiresIntegration reports whether expr can only be satisfied when the
// "integration" build tag is set: false under every assignment of the other
// tags with integration false, and true under at least one assignment with
// integration true. Checking whether "integration" appears in the expression
// at all is not enough: "!integration" and "linux || integration" both
// mention it but do not require it (the first forbids it, the second builds
// fine without it on linux), while a tag that never appears, such as in
// "!windows", never requires it either.
func requiresIntegration(e constraint.Expr) bool {
	tags := map[string]bool{}
	collectTags(e, tags)
	delete(tags, "integration")
	others := make([]string, 0, len(tags))
	for tag := range tags {
		others = append(others, tag)
	}
	assignment := func(bits int, integration bool) func(tag string) bool {
		vals := map[string]bool{"integration": integration}
		for i, tag := range others {
			vals[tag] = bits&(1<<i) != 0
		}
		return func(tag string) bool { return vals[tag] }
	}
	for bits := 0; bits < 1<<len(others); bits++ {
		if e.Eval(assignment(bits, false)) {
			return false
		}
	}
	for bits := 0; bits < 1<<len(others); bits++ {
		if e.Eval(assignment(bits, true)) {
			return true
		}
	}
	return false
}

// collectTags gathers every tag name that appears anywhere in expr.
func collectTags(e constraint.Expr, tags map[string]bool) {
	switch e := e.(type) {
	case *constraint.TagExpr:
		tags[e.Tag] = true
	case *constraint.NotExpr:
		collectTags(e.X, tags)
	case *constraint.AndExpr:
		collectTags(e.X, tags)
		collectTags(e.Y, tags)
	case *constraint.OrExpr:
		collectTags(e.X, tags)
		collectTags(e.Y, tags)
	}
}

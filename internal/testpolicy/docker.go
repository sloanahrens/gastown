package testpolicy

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// containerEntryPoints are the functions through which a test starts a
// container, or asks whether it may: the internal/testutil entry points and
// beads.RunTestContainerInit. A package whose unit-tier tests call one is
// Docker-backed and belongs in docker.txt.
var containerEntryPoints = map[string]bool{
	"RequireDoltContainer":           true,
	"EnsureDoltContainerForTestMain": true,
	"StartIsolatedDoltContainer":     true,
	"OpenTestStore":                  true,
	"TakePooledSQLDatabase":          true,
	"WithDolt":                       true,
	"DockerTestsEnabled":             true,
	"RunTestContainerInit":           true,
}

// StartsContainers returns the positions where the unit-tier test files of
// one package directory call a container entry point. Files excluded by build
// constraints with no extra tags are skipped, so an integration-tagged file
// never counts. It matches by function name, qualified or not, so a call from
// inside internal/testutil counts too.
func StartsContainers(dir string) ([]token.Position, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var calls []token.Position
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		ok, err := build.Default.MatchFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, name), err)
		}
		if !ok {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var fn string
			switch x := call.Fun.(type) {
			case *ast.Ident:
				fn = x.Name
			case *ast.SelectorExpr:
				fn = x.Sel.Name
			}
			if containerEntryPoints[fn] {
				calls = append(calls, fset.Position(call.Pos()))
			}
			return true
		})
	}
	return calls, nil
}

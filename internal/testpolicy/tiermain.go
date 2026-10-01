package testpolicy

import (
	"fmt"
	"go/ast"
	"path/filepath"
	"strings"
)

const unittierPkgPath = "github.com/steveyegge/gastown/internal/testutil/unittier"

// tierRunners are the TestMain bodies that check, when a package's tests
// end, what only a run can see (internal/testutil/unittier), by import path
// and function name.
var tierRunners = map[string]map[string]bool{
	testutilPkgPath: {"HermeticMain": true, "StartHermetic": true},
	unittierPkgPath: {"Main": true},
}

// UnitTierMain reports whether dir has unit-tier test files (those that
// build with no extra tags), and whether one of them calls a tierRunners
// function: testutil.HermeticMain, testutil.StartHermetic or unittier.Main,
// unqualified inside its own package.
func UnitTierMain(dir string) (hasTests, runs bool, err error) {
	_, files, err := unitTierTestFiles(dir)
	if err != nil {
		return false, false, err
	}
	for _, f := range files {
		if callsTierRunner(f) {
			return true, true, nil
		}
	}
	return len(files) > 0, false, nil
}

func callsTierRunner(f *ast.File) bool {
	imp := imports(f)
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		switch fn := c.Fun.(type) {
		case *ast.Ident:
			for path, names := range tierRunners {
				if fn.Obj == nil && names[fn.Name] && f.Name.Name == filepath.Base(path) {
					found = true
				}
			}
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok && x.Obj == nil && tierRunners[imp[x.Name]][fn.Sel.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

// CheckUnitTierMains returns one finding per package directory under root
// whose unit tier has test files but no TestMain running under a tierRunners
// function, so nothing checks its tests for goroutines that outlive them.
func CheckUnitTierMains(root string, dirs []string) ([]string, error) {
	var findings []string
	for _, dir := range dirs {
		rel := filepath.ToSlash(strings.TrimPrefix(dir, root+string(filepath.Separator)))
		hasTests, runs, err := UnitTierMain(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		if hasTests && !runs {
			findings = append(findings, fmt.Sprintf("%s: its unit tier has no TestMain that runs testutil.HermeticMain or unittier.Main, so nothing fails the run on a goroutine that outlives its tests", rel))
		}
	}
	return findings, nil
}

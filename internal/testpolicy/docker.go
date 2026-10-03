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

// containerEntryPoints are the functions through which a test starts a
// container, or asks whether it may: the internal/testutil entry points and
// beads.RunTestContainerInit. A package whose unit-tier tests call one is
// Docker-backed and belongs in docker.txt.
var containerEntryPoints = map[string]bool{
	"RequireDoltContainer":           true,
	"EnsureDoltContainerForTestMain": true,
	"LeaseScratchDoltContainer":      true,
	"LeaseScratchDoltContainerEnv":   true,
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

// CheckDockerTier compares the packages under root that start containers in
// their unit-tier tests with listed (docker.txt, repo-relative dirs) and
// returns one finding per mismatch:
//   - a package that calls an entry point but is not listed;
//   - a listed package that calls none;
//   - a listed path that is not a package directory under dirs;
//   - an exported production function or method that calls an entry point
//     without being one: a package calling it would start containers without
//     calling an entry point by name, so the list could not see it.
func CheckDockerTier(root string, dirs []string, listed map[string]bool) ([]string, error) {
	remaining := make(map[string]bool, len(listed))
	for rel := range listed {
		remaining[rel] = true
	}
	var findings, missing []string
	for _, dir := range dirs {
		rel := filepath.ToSlash(strings.TrimPrefix(dir, root+string(filepath.Separator)))
		calls, err := StartsContainers(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		switch {
		case len(calls) > 0 && !listed[rel]:
			missing = append(missing, fmt.Sprintf("%s (first call: %s) starts containers in its unit-tier tests but is not in docker.txt, so make test-integration never runs them: add it", rel, calls[0]))
		case len(calls) == 0 && listed[rel]:
			findings = append(findings, fmt.Sprintf("docker.txt lists %s, whose unit-tier tests call no container entry point: delete its line", rel))
		}
		delete(remaining, rel)
		wrappers, err := exportedEntryWrappers(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		findings = append(findings, wrappers...)
	}
	sort.Strings(missing)
	findings = append(findings, missing...)
	stale := make([]string, 0, len(remaining))
	for rel := range remaining {
		stale = append(stale, rel)
	}
	sort.Strings(stale)
	for _, rel := range stale {
		findings = append(findings, fmt.Sprintf("docker.txt lists %s, which is not a Go package directory", rel))
	}
	return findings, nil
}

// exportedEntryWrappers reports the exported functions and methods in one
// directory's production files that call a container entry point and are not
// entry points themselves.
func exportedEntryWrappers(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
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
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !fn.Name.IsExported() || containerEntryPoints[fn.Name.Name] {
				continue
			}
			if callsEntryPoint(fn.Body) {
				out = append(out, fmt.Sprintf("%s: exported %s calls a container entry point but is not one: add it to containerEntryPoints in internal/testpolicy/docker.go, or unexport it", fset.Position(fn.Name.Pos()), fn.Name.Name))
			}
		}
	}
	return out, nil
}

// callsEntryPoint reports whether n contains a call to a container entry
// point that starts or leases one, by function name, qualified or not.
// DockerTestsEnabled only reads the opt-in, so calling it does not make a
// wrapper: testutil.StartHermetic reads it and starts a container only when
// given WithDolt, which is an entry point in its own right.
func callsEntryPoint(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		var fn string
		switch x := call.Fun.(type) {
		case *ast.Ident:
			fn = x.Name
		case *ast.SelectorExpr:
			fn = x.Sel.Name
		}
		found = containerEntryPoints[fn] && fn != "DockerTestsEnabled"
		return !found
	})
	return found
}

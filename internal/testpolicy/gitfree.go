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

// RuleNoGit is broken by a unit-tier test in a package listed in gitfree.txt
// that runs git, or builds the real git wrapper that would run it.
const RuleNoGit = "no-git"

const (
	gitPkgPath      = "github.com/steveyegge/gastown/internal/git"
	testutilPkgPath = "github.com/steveyegge/gastown/internal/testutil"
)

// realGitFuncs are the package git functions that run git on PATH: the two
// constructors of a *git.Git with no scripted runner, and the helpers that
// call realRun themselves.
var realGitFuncs = map[string]bool{
	"NewGit": true, "NewGitWithDir": true,
	"InitSubmodules": true, "InitSparseCheckout": true, "EnsureSafeMutationWorkDir": true,
}

// GitFreeFindings checks the unit-tier test files of one package directory
// against the no-git rule: no exec.Command("git"), no call to a package git
// function that runs real git (realGitFuncs, unqualified inside package git
// itself), and no call to a test helper of the same package that does one
// of those, directly or through another helper. It also reports whether a
// unit-tier file calls testutil.WithoutGit (unqualified inside package
// testutil itself), which the package's TestMain must pass so git reached
// through production code fails too. Files excluded by build constraints
// with no extra tags are skipped, so an integration-tagged file never
// counts.
//
// Helpers are matched by name among the package's top-level test functions;
// a helper method, or a function value stored in a variable, is not
// followed, and neither is git that production code runs (WithoutGit
// catches that at run time).
func GitFreeFindings(dir string) (vs []Violation, withoutGit bool, err error) {
	fset, files, err := unitTierTestFiles(dir)
	if err != nil {
		return nil, false, err
	}
	var direct []Violation
	for _, f := range files {
		fileVs, wg := directGitCalls(fset, f)
		direct = append(direct, fileVs...)
		withoutGit = withoutGit || wg
	}
	// A helper is a top-level test function whose body holds a surviving
	// no-git violation. Calls to helpers are violations too, so the set
	// grows until no new helper appears.
	helpers := map[helperKey]token.Position{}
	for {
		vs = append([]Violation(nil), direct...)
		for _, f := range files {
			vs = append(vs, helperCalls(fset, f, helpers)...)
		}
		vs, _ = applyAllows(fset, files, vs)
		grew := false
		for _, f := range files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv != nil || fd.Body == nil {
					continue
				}
				k := helperKey{f.Name.Name, fd.Name.Name}
				if _, seen := helpers[k]; seen {
					continue
				}
				if v, ok := firstNoGitWithin(fset, fd, vs); ok {
					helpers[k] = v.Pos
					grew = true
				}
			}
		}
		if !grew {
			break
		}
	}
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].Pos.Filename != vs[j].Pos.Filename {
			return vs[i].Pos.Filename < vs[j].Pos.Filename
		}
		if vs[i].Pos.Line != vs[j].Pos.Line {
			return vs[i].Pos.Line < vs[j].Pos.Line
		}
		return vs[i].Pos.Column < vs[j].Pos.Column
	})
	return vs, withoutGit, nil
}

// unitTierTestFiles parses dir's test files that build with no extra tags:
// its unit tier.
func unitTierTestFiles(dir string) (*token.FileSet, []*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		ok, err := build.Default.MatchFile(dir, name)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", filepath.Join(dir, name), err)
		}
		if !ok {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, f)
	}
	return fset, files, nil
}

// helperKey names a top-level function of one package clause: package foo
// and its external foo_test share a directory but not their identifiers.
type helperKey struct{ pkg, name string }

// directGitCalls returns the calls in f that run git themselves, and whether
// f calls testutil.WithoutGit.
func directGitCalls(fset *token.FileSet, f *ast.File) (vs []Violation, withoutGit bool) {
	imp := imports(f)
	add := func(n ast.Node, msg string) {
		vs = append(vs, Violation{Pos: fset.Position(n.Pos()), Rule: RuleNoGit, Msg: msg})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := c.Fun.(type) {
		case *ast.Ident:
			if f.Name.Name == "git" && fn.Obj == nil && realGitFuncs[fn.Name] {
				add(c, fn.Name+" runs real git; give it a scripted runner")
			}
			if f.Name.Name == "testutil" && fn.Obj == nil && fn.Name == "WithoutGit" {
				withoutGit = true // testutil's own TestMain, unqualified
			}
		case *ast.SelectorExpr:
			x, ok := fn.X.(*ast.Ident)
			if !ok || x.Obj != nil {
				return true
			}
			switch path := imp[x.Name]; {
			case path == gitPkgPath && realGitFuncs[fn.Sel.Name]:
				add(c, "git."+fn.Sel.Name+" runs real git; use gitfake")
			case path == testutilPkgPath && fn.Sel.Name == "WithoutGit":
				withoutGit = true
			case path == "os/exec" && (fn.Sel.Name == "Command" || fn.Sel.Name == "CommandContext"):
				i := 0
				if fn.Sel.Name == "CommandContext" {
					i = 1
				}
				if len(c.Args) > i && stringLit(c.Args[i]) == "git" {
					add(c, "runs git; use gitfake or canned output, or move the test to the integration tier")
				}
			}
		}
		return true
	})
	return vs, withoutGit
}

// helperCalls returns the calls in f to a known git-running helper of f's
// package. An identifier that resolves to a local variable or parameter is
// not a call to the helper of the same name.
func helperCalls(fset *token.FileSet, f *ast.File, helpers map[helperKey]token.Position) []Violation {
	var vs []Violation
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := c.Fun.(*ast.Ident)
		if !ok || (id.Obj != nil && id.Obj.Kind != ast.Fun) {
			return true
		}
		if at, ok := helpers[helperKey{f.Name.Name, id.Name}]; ok {
			vs = append(vs, Violation{Pos: fset.Position(c.Pos()), Rule: RuleNoGit,
				Msg: fmt.Sprintf("calls %s, which runs git (%s:%d)", id.Name, filepath.Base(at.Filename), at.Line)})
		}
		return true
	})
	return vs
}

// firstNoGitWithin returns the first no-git violation inside fd's body.
func firstNoGitWithin(fset *token.FileSet, fd *ast.FuncDecl, vs []Violation) (Violation, bool) {
	from, to := fset.Position(fd.Body.Pos()), fset.Position(fd.Body.End())
	for _, v := range vs {
		if v.Rule == RuleNoGit && v.Pos.Filename == from.Filename && v.Pos.Offset >= from.Offset && v.Pos.Offset < to.Offset {
			return v, true
		}
	}
	return Violation{}, false
}

// CheckGitFree applies the no-git rule to the packages listed in gitfree.txt
// (repo-relative dirs) and returns one finding per problem:
//   - a no-git violation in a listed package's unit-tier tests;
//   - a listed package whose unit-tier tests never call testutil.WithoutGit;
//   - a listed path that is not a package directory under dirs.
//
// Packages not listed are not checked: the list grows one converted package
// at a time.
func CheckGitFree(root string, dirs []string, listed map[string]bool) ([]string, error) {
	remaining := make(map[string]bool, len(listed))
	for rel := range listed {
		remaining[rel] = true
	}
	var findings []string
	for _, dir := range dirs {
		rel := filepath.ToSlash(strings.TrimPrefix(dir, root+string(filepath.Separator)))
		if !listed[rel] {
			continue
		}
		delete(remaining, rel)
		vs, withoutGit, err := GitFreeFindings(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		for _, v := range vs {
			findings = append(findings, v.String())
		}
		if !withoutGit {
			findings = append(findings, fmt.Sprintf("gitfree.txt lists %s, but its unit-tier TestMain does not pass testutil.WithoutGit(), so git reached through production code would still run", rel))
		}
	}
	stale := make([]string, 0, len(remaining))
	for rel := range remaining {
		stale = append(stale, rel)
	}
	sort.Strings(stale)
	for _, rel := range stale {
		findings = append(findings, fmt.Sprintf("gitfree.txt lists %s, which is not a Go package directory", rel))
	}
	return findings, nil
}

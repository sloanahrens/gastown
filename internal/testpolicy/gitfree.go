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

// realGitConstructors build a *git.Git that runs git on PATH.
var realGitConstructors = map[string]bool{"NewGit": true, "NewGitWithDir": true}

// GitFreeFindings checks the unit-tier test files of one package directory
// against the no-git rule: no exec.Command("git"), and no git.NewGit or
// git.NewGitWithDir (unqualified inside package git itself). It also
// reports whether a unit-tier file calls testutil.WithoutGit, which the
// package's TestMain must pass so git reached through production code fails
// too. Files excluded by build constraints with no extra tags are skipped,
// so an integration-tagged file never counts.
func GitFreeFindings(dir string) (vs []Violation, withoutGit bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, err
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		ok, err := build.Default.MatchFile(dir, name)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", filepath.Join(dir, name), err)
		}
		if !ok {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, false, err
		}
		imp := imports(f)
		var fileVs []Violation
		add := func(n ast.Node, msg string) {
			fileVs = append(fileVs, Violation{Pos: fset.Position(n.Pos()), Rule: RuleNoGit, Msg: msg})
		}
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := c.Fun.(type) {
			case *ast.Ident:
				if f.Name.Name == "git" && fn.Obj == nil && realGitConstructors[fn.Name] {
					add(c, fn.Name+" builds a Git that runs real git; give it a scripted runner")
				}
			case *ast.SelectorExpr:
				x, ok := fn.X.(*ast.Ident)
				if !ok || x.Obj != nil {
					return true
				}
				switch path := imp[x.Name]; {
				case path == gitPkgPath && realGitConstructors[fn.Sel.Name]:
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
		fileVs, _ = applyAllows(fset, []*ast.File{f}, fileVs)
		vs = append(vs, fileVs...)
	}
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].Pos.Filename != vs[j].Pos.Filename {
			return vs[i].Pos.Filename < vs[j].Pos.Filename
		}
		return vs[i].Pos.Line < vs[j].Pos.Line
	})
	return vs, withoutGit, nil
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

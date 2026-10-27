package config

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A stat error that is not absence on a legacy directive file is an error the
// read reports, not an absent file (gt-80rnu). The legacy town directory is a
// regular file here, so stat of a file beneath it fails with ENOTDIR, not
// ErrNotExist; a reader that treated that as absence would render no directive
// and pass.
func TestDirectiveLegacyStatErrorFailsClosed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, "directives"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRoleDirectives("polecat", town, "alpha"); err == nil {
		t.Fatal("ReadRoleDirectives returned no error for a legacy stat failure that is not absence")
	}
}

// directoryPathOffences returns the line of each place in the Go source src that
// builds a directory path ending in "directives": a call to filepath.Join or
// path.Join with a string literal argument that is "directives", ends in
// "/directives" or starts with "directives/", or a "+" concatenation with such a
// literal. Arguments can be calls, so the match is on the syntax tree, not on
// the text of a line (gt-80rnu).
func directoryPathOffences(src string) ([]int, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		return nil, err
	}
	var lines []int
	isDirectiveLit := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return false
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			return false
		}
		return v == "directives" || strings.HasSuffix(v, "/directives") || strings.HasPrefix(v, "directives/")
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || sel.Sel.Name != "Join" || (pkg.Name != "filepath" && pkg.Name != "path") {
				return true
			}
			for _, arg := range x.Args {
				if isDirectiveLit(arg) {
					lines = append(lines, fset.Position(x.Pos()).Line)
					break
				}
			}
		case *ast.BinaryExpr:
			if x.Op == token.ADD && (isDirectiveLit(x.X) || isDirectiveLit(x.Y)) {
				lines = append(lines, fset.Position(x.Pos()).Line)
			}
		}
		return true
	})
	return lines, nil
}

// TestDirectoryPathMatcher pins the matcher on fixtures: a join whose other
// argument is a call (the case a line regexp could not see), a plain join, a
// concatenation, and two that must not match.
func TestDirectoryPathMatcher(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  string
		want int
	}{
		{"join with a call argument", "package p\nvar _ = filepath.Join(rigPath(x), \"directives\")\n", 1},
		{"plain join", "package p\nvar _ = filepath.Join(townRoot, \"directives\", role+\".md\")\n", 1},
		{"path join of a suffix", "package p\nvar _ = path.Join(rig, \"x/directives\")\n", 1},
		{"concatenation", "package p\nvar _ = rig + \"/directives\"\n", 1},
		{"a join of other names", "package p\nvar _ = filepath.Join(townRoot, \"rig\", \"file.md\")\n", 0},
		{"a directives word that is not a path", "package p\nvar _ = p.add(\"directives\", 3)\n", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := directoryPathOffences(tc.src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("offences = %v, want %d", got, tc.want)
			}
		})
	}
}

// TestNoDirectivePathBuiltOutsideConfig: every directive path is built by the
// config package (directivePaths, CentralDirectiveDir, RigDirectiveDir and the
// helpers beside them), so a Go file outside it that joins a path ending in
// "directives" fails here. The allowlist starts empty (gt-80rnu).
func TestNoDirectivePathBuiltOutsideConfig(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if d.IsDir() {
				if filepath.ToSlash(rel) == "internal/config" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			lines, err := directoryPathOffences(string(data))
			if err != nil {
				return fmt.Errorf("parsing %s: %w", rel, err)
			}
			for _, line := range lines {
				offenders = append(offenders, filepath.ToSlash(rel)+":"+strconv.Itoa(line))
			}
			return nil
		})
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("directive paths built outside internal/config (use config.DirectivePaths, DirectiveEditPath or DirectiveTreeRels): %s", strings.Join(offenders, ", "))
	}
}

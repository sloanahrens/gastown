package ui

import (
	"bytes"
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

// The two packages whose HasDarkBackground queries the terminal with ESC]11;?
// (and ESC[6n to read the cursor), which a shell hook then evals as a command.
const (
	termenvImport  = "github.com/muesli/termenv"
	lipglossImport = "github.com/charmbracelet/lipgloss"
)

// TestNoTerminalBackgroundProbe: no non-test file under internal/ calls
// termenv.HasDarkBackground or lipgloss.HasDarkBackground. The one background
// answer comes from uiEnv.detectDarkBackground, which reads COLORFGBG and never
// queries the terminal (gt-xvekee).
func TestNoTerminalBackgroundProbe(t *testing.T) {
	t.Parallel()
	var hits []string
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		h, err := backgroundProbeCalls(path, src)
		if err != nil {
			return err
		}
		hits = append(hits, h...)
		return nil
	})
	if err != nil {
		t.Fatalf("reading the internal source tree: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("these query the terminal for its background; answer through uiEnv.detectDarkBackground instead:\n%s", strings.Join(hits, "\n"))
	}
}

// backgroundProbeCalls returns the position of each reference to
// termenv.HasDarkBackground or lipgloss.HasDarkBackground in one source file,
// call or method value alike. The package name is matched through the import,
// so an aliased import counts and an unrelated package with the same method
// name does not.
func backgroundProbeCalls(path string, src []byte) ([]string, error) {
	if !bytes.Contains(src, []byte("HasDarkBackground")) {
		return nil, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	probePkgs := map[string]bool{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || (p != termenvImport && p != lipglossImport) {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		probePkgs[name] = true
	}
	if len(probePkgs) == 0 {
		return nil, nil
	}
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HasDarkBackground" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && probePkgs[pkg.Name] {
			hits = append(hits, fset.Position(sel.Pos()).String())
		}
		return true
	})
	return hits, nil
}

// TestBackgroundProbeCalls drives the guard with the reference shapes it must
// catch, the lookalikes it must leave alone, and a file it cannot read.
func TestBackgroundProbeCalls(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		src     string
		want    int
		wantErr bool
	}{
		{"termenv call", `package x
import "github.com/muesli/termenv"
func f() bool { return termenv.HasDarkBackground() }`, 1, false},
		{"lipgloss call", `package x
import "github.com/charmbracelet/lipgloss"
func f() bool { return lipgloss.HasDarkBackground() }`, 1, false},
		{"aliased import", `package x
import t "github.com/muesli/termenv"
func f() bool { return t.HasDarkBackground() }`, 1, false},
		{"method value", `package x
import l "github.com/charmbracelet/lipgloss"
var f = l.HasDarkBackground`, 1, false},
		{"setter not flagged", `package x
import "github.com/charmbracelet/lipgloss"
func f(b bool) { lipgloss.SetHasDarkBackground(b) }`, 0, false},
		{"same method in another package", `package x
import "example.com/other/ui"
func f() bool { return ui.HasDarkBackground() }`, 0, false},
		{"unrelated package imported as termenv", `package x
import termenv "example.com/something"
func f() bool { return termenv.HasDarkBackground() }`, 0, false},
		{"unparseable source", `package x
import "github.com/muesli/termenv"
func f( { termenv.HasDarkBackground() }`, 0, true},
	}
	for _, tc := range cases {
		hits, err := backgroundProbeCalls("x.go", []byte(tc.src))
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: backgroundProbeCalls returned nil error, want a parse error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: backgroundProbeCalls: %v", tc.name, err)
		}
		if len(hits) != tc.want {
			t.Errorf("%s: backgroundProbeCalls returned %d hits, want %d", tc.name, len(hits), tc.want)
		}
	}
}

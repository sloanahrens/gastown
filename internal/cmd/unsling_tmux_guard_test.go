package cmd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// tmuxScope names the code gt unsling resolves its target through, which must
// never reach tmux. wholeFile covers every declaration in the file; otherwise
// only the named functions are covered. funcs are also the floor: the scan must
// find each one, or it has gone blind.
type tmuxScope struct {
	file      string
	wholeFile bool
	funcs     []string
}

// unslingTmuxScopes is the guarded code. unsling.go never asks tmux. In
// sling_target.go only the id path is guarded: resolveTargetAgent in the same
// file asks tmux for the pane and working directory on purpose, so the scope is
// by function, not by file.
var unslingTmuxScopes = []tmuxScope{
	{file: "unsling.go", wholeFile: true, funcs: []string{"runUnslingDeps", "unslingHook", "reopenBead"}},
	{file: "sling_target.go", funcs: []string{"resolveTargetSession", "resolveTargetAgentID", "sessionToAgentID"}},
}

// tmuxReachNames are the identifiers resolveTargetAgent reaches tmux through,
// besides the pane and working-directory helpers matched by name.
var tmuxReachNames = map[string]bool{
	"resolveTargetAgent":   true,
	"resolveTargetAgentFn": true,
}

// tmuxReach is one reference in guarded code to a tmux, pane or
// working-directory helper.
type tmuxReach struct {
	fn   string
	name string
	line int
}

// reachesTmux reports whether an identifier names a pane or working-directory
// helper, matched without case so a getSessionPane or GetPaneWorkDir is caught.
func reachesTmux(name string) bool {
	lower := strings.ToLower(name)
	return tmuxReachNames[name] || strings.Contains(lower, "pane") || strings.Contains(lower, "workdir")
}

// scanTmuxReaches parses src, the Go source of file, and reports every reference
// inside sc's scope to a tmux helper. It also returns the names of the functions
// the file declares, so a caller can check the scan saw what it must.
func scanTmuxReaches(file string, src []byte, sc tmuxScope) ([]tmuxReach, []string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", file, err)
	}

	var declared []string
	var reaches []tmuxReach
	inspect := func(fn string, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				// A call through the tmux package is a reach whatever its name.
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "tmux" {
					reaches = append(reaches, tmuxReach{fn, "tmux." + x.Sel.Name, fset.Position(x.Pos()).Line})
					return false
				}
			case *ast.Ident:
				if reachesTmux(x.Name) {
					reaches = append(reaches, tmuxReach{fn, x.Name, fset.Position(x.Pos()).Line})
				}
			}
			return true
		})
	}

	for _, d := range f.Decls {
		fd, isFunc := d.(*ast.FuncDecl)
		if isFunc {
			declared = append(declared, fd.Name.Name)
		}
		switch {
		case isFunc && (sc.wholeFile || slices.Contains(sc.funcs, fd.Name.Name)):
			inspect(fd.Name.Name, fd)
		case !isFunc && sc.wholeFile:
			inspect("(file scope)", d)
		}
	}
	return reaches, declared, nil
}

// TestUnslingSourceNeverReachesTmux fails when the code gt unsling resolves its
// target through names a pane or working-directory helper, a tmux call, or
// resolveTargetAgent.
func TestUnslingSourceNeverReachesTmux(t *testing.T) {
	t.Parallel()
	for _, sc := range unslingTmuxScopes {
		src, err := os.ReadFile(sc.file)
		if err != nil {
			t.Fatal(err)
		}
		reaches, _, err := scanTmuxReaches(sc.file, src, sc)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range reaches {
			t.Errorf("%s:%d: %s calls %s; unsling must resolve its target without tmux", sc.file, r.line, r.fn, r.name)
		}
	}
}

// TestUnslingTmuxScanReportsAnAddedCall feeds the scanner snippets that add a
// pane call. A call added to a guarded function is reported with its function;
// the same call added to resolveTargetAgent is outside the scope and is not.
func TestUnslingTmuxScanReportsAnAddedCall(t *testing.T) {
	t.Parallel()
	sling := unslingTmuxScopes[1]
	unsling := unslingTmuxScopes[0]

	for _, tc := range []struct {
		name string
		sc   tmuxScope
		src  string
		want []string
	}{
		{
			name: "pane call added to a guarded function is reported",
			sc:   sling,
			src: `package cmd

func resolveTargetAgentID(reg *session.PrefixRegistry, target string) (string, error) {
	_, err := getSessionPane(target)
	return "", err
}
`,
			want: []string{"resolveTargetAgentID calls getSessionPane"},
		},
		{
			name: "same call in resolveTargetAgent is outside the scope",
			sc:   sling,
			src: `package cmd

func resolveTargetAgent(target string) (string, error) {
	_, err := getSessionPane(target)
	return "", err
}
`,
			want: nil,
		},
		{
			name: "tmux package call added to unsling.go is reported",
			sc:   unsling,
			src: `package cmd

func unslingExtra() {
	_ = tmux.NewTmux()
}
`,
			want: []string{"unslingExtra calls tmux.NewTmux"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reaches, _, err := scanTmuxReaches(tc.sc.file, []byte(tc.src), tc.sc)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range reaches {
				got = append(got, fmt.Sprintf("%s calls %s", r.fn, r.name))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("scan reported %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUnslingTmuxScanRefusesUnparseableSource: a source the scanner cannot read
// is an error, never an empty report that would pass as clean.
func TestUnslingTmuxScanRefusesUnparseableSource(t *testing.T) {
	t.Parallel()
	const broken = "package cmd\n\nfunc resolveTargetAgentID( {\n"

	_, _, err := scanTmuxReaches("sling_target.go", []byte(broken), unslingTmuxScopes[1])
	if err == nil || !strings.Contains(err.Error(), "sling_target.go") {
		t.Fatalf("scanTmuxReaches on unparseable source = %v, want an error naming sling_target.go", err)
	}
}

// TestUnslingTmuxScanSeesTheGuardedFunctions is the floor: the scan must read
// each guarded file and find every function the scope names. A scan that has
// gone blind reports nothing and would pass the source test.
func TestUnslingTmuxScanSeesTheGuardedFunctions(t *testing.T) {
	t.Parallel()
	for _, sc := range unslingTmuxScopes {
		src, err := os.ReadFile(sc.file)
		if err != nil {
			t.Fatalf("reading %s: %v", sc.file, err)
		}
		_, declared, err := scanTmuxReaches(sc.file, src, sc)
		if err != nil {
			t.Fatal(err)
		}
		for _, fn := range sc.funcs {
			if !slices.Contains(declared, fn) {
				t.Errorf("scan of %s did not find %s; the scanner has stopped seeing it", sc.file, fn)
			}
		}
	}
}

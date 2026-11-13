package cmd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"
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

// tmuxReachWords are the camelCase words that name a pane or
// working-directory helper. A whole word is matched, not a substring, so
// panelWidth and companyPanel are not reaches while getSessionPane and
// GetPaneWorkDir are.
var tmuxReachWords = map[string]bool{
	"pane":     true,
	"panes":    true,
	"workdir":  true,
	"workdirs": true,
}

// tmuxReach is one reference in guarded code to a tmux, pane or
// working-directory helper.
type tmuxReach struct {
	file string
	fn   string
	name string
	line int
}

// tmuxPackage is the Go source of every file one scan walks, keyed by file
// name. Following a call needs the callee's source, which may live in another
// file.
type tmuxPackage map[string][]byte

// readTmuxPackage reads the non-test Go sources in dir, the package a scan
// follows calls through.
func readTmuxPackage(t *testing.T, dir string) tmuxPackage {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	pkg := tmuxPackage{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		pkg[name] = src
	}
	if len(pkg) == 0 {
		t.Fatalf("no Go sources in %s", dir)
	}
	return pkg
}

// identWords splits a Go identifier into its lower-cased camelCase words:
// getSessionPane is get session pane, GetPaneWorkDir is get pane work dir.
func identWords(name string) []string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if unicode.IsUpper(r) {
			flush()
		}
		cur.WriteRune(r)
	}
	flush()
	return words
}

// paneHelperName reports whether name spells a pane or working-directory helper,
// matched by camelCase word so getSessionPane and GetPaneWorkDir are caught and
// panelWidth is not.
func paneHelperName(name string) bool {
	return slices.ContainsFunc(identWords(name), func(w string) bool { return tmuxReachWords[w] })
}

// tmuxScan is one scan's state: the package functions a call may lead to, the
// file each is declared in, and the functions already walked, so a cycle ends.
type tmuxScan struct {
	fset     *token.FileSet
	funcs    map[string][]*ast.FuncDecl
	declFile map[*ast.FuncDecl]string
	visited  map[*ast.FuncDecl]bool
	reaches  []tmuxReach
}

// follow walks the body of every package function name declares, unless the
// name is itself a reach: that is already reported, and walking it would report
// the tmux call inside the helper the guard exists to catch.
func (s *tmuxScan) follow(name string) {
	if tmuxReachNames[name] || paneHelperName(name) {
		return
	}
	for _, fd := range s.funcs[name] {
		if s.visited[fd] {
			continue
		}
		s.visited[fd] = true
		s.walk(s.declFile[fd], name, fd)
	}
}

// walk records every tmux reach under root, which file declares as fn, and
// follows each package function root calls, so a reach one hop away is
// reported too. A reach is a call to a pane or working-directory helper, a call
// through the tmux package, or any reference to a name in tmuxReachNames: a
// variable that happens to be called pane is none of them.
func (s *tmuxScan) walk(file, fn string, root ast.Node) {
	report := func(name string, pos token.Pos) {
		s.reaches = append(s.reaches, tmuxReach{file, fn, name, s.fset.Position(pos).Line})
	}
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			// A call through the tmux package is a reach whatever its name.
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "tmux" {
				report("tmux."+x.Sel.Name, x.Pos())
				return false
			}
			// A selector's Sel is a field or method, never a package function:
			// walk the receiver only.
			ast.Inspect(x.X, visit)
			return false
		case *ast.CallExpr:
			id, ok := x.Fun.(*ast.Ident)
			if !ok {
				// A method or package call: the selector rule above covers it.
				return true
			}
			if paneHelperName(id.Name) {
				report(id.Name, id.Pos())
			} else {
				s.follow(id.Name)
			}
			// The callee is judged here, so walk the arguments only.
			for _, a := range x.Args {
				ast.Inspect(a, visit)
			}
			return false
		case *ast.Ident:
			if tmuxReachNames[x.Name] {
				report(x.Name, x.Pos())
			}
		}
		return true
	}
	ast.Inspect(root, visit)
}

// scanTmuxReaches parses pkg and reports every reference inside sc's scope to a
// tmux helper, following calls into the package functions it names. It also
// returns the names of the functions sc's file declares, so a caller can check
// the scan saw what it must.
func scanTmuxReaches(pkg tmuxPackage, sc tmuxScope) ([]tmuxReach, []string, error) {
	fset := token.NewFileSet()
	files := make(map[string]*ast.File, len(pkg))
	funcs := map[string][]*ast.FuncDecl{}
	declFile := map[*ast.FuncDecl]string{}
	for _, name := range slices.Sorted(maps.Keys(pkg)) {
		f, err := parser.ParseFile(fset, name, pkg[name], parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		files[name] = f
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = append(funcs[fd.Name.Name], fd)
				declFile[fd] = name
			}
		}
	}

	f, ok := files[sc.file]
	if !ok {
		return nil, nil, fmt.Errorf("scope names %s, which the package does not hold", sc.file)
	}

	s := &tmuxScan{fset: fset, funcs: funcs, declFile: declFile, visited: map[*ast.FuncDecl]bool{}}
	var declared []string
	for _, d := range f.Decls {
		fd, isFunc := d.(*ast.FuncDecl)
		if isFunc {
			declared = append(declared, fd.Name.Name)
		}
		switch {
		case isFunc && (sc.wholeFile || slices.Contains(sc.funcs, fd.Name.Name)):
			s.visited[fd] = true
			s.walk(sc.file, fd.Name.Name, fd)
		case !isFunc && sc.wholeFile:
			s.walk(sc.file, "(file scope)", d)
		}
	}
	return s.reaches, declared, nil
}

// TestUnslingSourceNeverReachesTmux fails when the code gt unsling resolves its
// target through names a pane or working-directory helper, a tmux call, or
// resolveTargetAgent, including through a package function it calls.
func TestUnslingSourceNeverReachesTmux(t *testing.T) {
	t.Parallel()
	pkg := readTmuxPackage(t, ".")
	for _, sc := range unslingTmuxScopes {
		reaches, _, err := scanTmuxReaches(pkg, sc)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range reaches {
			t.Errorf("%s:%d: %s reaches %s; unsling must resolve its target without tmux", r.file, r.line, r.fn, r.name)
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
		pkg  tmuxPackage
		sc   tmuxScope
		want []string
	}{
		{
			name: "pane call added to a guarded function is reported",
			pkg: tmuxPackage{"sling_target.go": []byte(`package cmd

func resolveTargetAgentID(reg *session.PrefixRegistry, target string) (string, error) {
	_, err := getSessionPane(target)
	return "", err
}
`)},
			sc:   sling,
			want: []string{"resolveTargetAgentID reaches getSessionPane"},
		},
		{
			name: "same call in resolveTargetAgent is outside the scope",
			pkg: tmuxPackage{"sling_target.go": []byte(`package cmd

func resolveTargetAgent(target string) (string, error) {
	_, err := getSessionPane(target)
	return "", err
}
`)},
			sc:   sling,
			want: nil,
		},
		{
			name: "tmux package call added to unsling.go is reported",
			pkg: tmuxPackage{"unsling.go": []byte(`package cmd

func unslingExtra() {
	_ = tmux.NewTmux()
}
`)},
			sc:   unsling,
			want: []string{"unslingExtra reaches tmux.NewTmux"},
		},
		{
			name: "a local variable named pane is not a reach",
			pkg: tmuxPackage{"sling_target.go": []byte(`package cmd

func resolveTargetAgentID(reg *session.PrefixRegistry, target string) (string, error) {
	pane := target
	if pane == "" {
		return "", nil
	}
	return pane, nil
}
`)},
			sc:   sling,
			want: nil,
		},
		{
			name: "a reference to resolveTargetAgent is reported without a call",
			pkg: tmuxPackage{"sling_target.go": []byte(`package cmd

func resolveTargetAgentID(reg *session.PrefixRegistry, target string) (string, error) {
	resolve := resolveTargetAgent
	_, _, _, err := resolve(target)
	return "", err
}
`)},
			sc:   sling,
			want: []string{"resolveTargetAgentID reaches resolveTargetAgent"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reaches, _, err := scanTmuxReaches(tc.pkg, tc.sc)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range reaches {
				got = append(got, fmt.Sprintf("%s reaches %s", r.fn, r.name))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("scan reported %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUnslingTmuxScanFollowsCalls: a guarded function that calls a package
// function is guarded through it. A pane call one call away is reported with
// the helper that makes it, across files too, and a call cycle ends.
func TestUnslingTmuxScanFollowsCalls(t *testing.T) {
	t.Parallel()
	sling := unslingTmuxScopes[1]

	for _, tc := range []struct {
		name string
		pkg  tmuxPackage
		want []string
	}{
		{
			name: "helper in the same file",
			pkg: tmuxPackage{"sling_target.go": []byte(`package cmd

func resolveTargetAgentID(reg *session.PrefixRegistry, target string) (string, error) {
	return helper(target), nil
}

func helper(target string) string {
	_, err := getSessionPane(target)
	return err.Error()
}
`)},
			want: []string{"helper reaches getSessionPane"},
		},
		{
			name: "helper in another file",
			pkg: tmuxPackage{
				"sling_target.go": []byte(`package cmd

func resolveTargetAgentID(reg *session.PrefixRegistry, target string) (string, error) {
	return helper(target), nil
}
`),
				"handoff.go": []byte(`package cmd

func helper(target string) string {
	_, err := getSessionPane(target)
	return err.Error()
}
`),
			},
			want: []string{"helper reaches getSessionPane"},
		},
		{
			name: "call cycle ends",
			pkg: tmuxPackage{"sling_target.go": []byte(`package cmd

func resolveTargetAgentID(reg *session.PrefixRegistry, target string) (string, error) {
	return helperA(target), nil
}

func helperA(target string) string { return helperB(target) }

func helperB(target string) string {
	_, err := getSessionPane(target)
	if nil != err {
		return helperA(target)
	}
	return ""
}
`)},
			want: []string{"helperB reaches getSessionPane"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reaches, _, err := scanTmuxReaches(tc.pkg, sling)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range reaches {
				got = append(got, fmt.Sprintf("%s reaches %s", r.fn, r.name))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("scan reported %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTmuxReachWords: a helper is named by a whole camelCase word, not by a
// substring, so a panel or a company name is not one.
func TestTmuxReachWords(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want bool
	}{
		{name: "getSessionPane", want: true},
		{name: "GetPaneWorkDir", want: true},
		{name: "pane", want: true},
		{name: "listPanes", want: true},
		{name: "workdirPath", want: true},
		{name: "panelWidth", want: false},
		{name: "companyPanel", want: false},
		{name: "panel", want: false},
	} {
		if got := paneHelperName(tc.name); got != tc.want {
			t.Errorf("paneHelperName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestUnslingTmuxScanRefusesUnparseableSource: a source the scanner cannot read
// is an error, never an empty report that would pass as clean.
func TestUnslingTmuxScanRefusesUnparseableSource(t *testing.T) {
	t.Parallel()
	const broken = "package cmd\n\nfunc resolveTargetAgentID( {\n"

	_, _, err := scanTmuxReaches(tmuxPackage{"sling_target.go": []byte(broken)}, unslingTmuxScopes[1])
	if err == nil || !strings.Contains(err.Error(), "sling_target.go") {
		t.Fatalf("scanTmuxReaches on unparseable source = %v, want an error naming sling_target.go", err)
	}
}

// TestUnslingTmuxScanRefusesAMissingScopeFile: a scope naming a file the
// package does not hold is an error, never an empty scan of nothing.
func TestUnslingTmuxScanRefusesAMissingScopeFile(t *testing.T) {
	t.Parallel()

	pkg := tmuxPackage{"unsling.go": []byte("package cmd\n")}
	_, _, err := scanTmuxReaches(pkg, unslingTmuxScopes[1])
	if err == nil || !strings.Contains(err.Error(), "sling_target.go") {
		t.Fatalf("scanTmuxReaches with sling_target.go missing = %v, want an error naming sling_target.go", err)
	}
}

// TestUnslingTmuxScanSeesTheGuardedFunctions is the floor: the scan must read
// each guarded file and find every function the scope names. A scan that has
// gone blind reports nothing and would pass the source test.
func TestUnslingTmuxScanSeesTheGuardedFunctions(t *testing.T) {
	t.Parallel()
	pkg := readTmuxPackage(t, ".")
	for _, sc := range unslingTmuxScopes {
		_, declared, err := scanTmuxReaches(pkg, sc)
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

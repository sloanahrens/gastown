package git

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The outcome checks below ask git whether a ref exists. git answers 0 for a
// ref it has and 1 for one it does not, so exit 1 is an ordinary "no". Every
// other failure exits non-zero too, and a fatal one exits 128, whose text
// "exit status 128" contains "exit status 1": matching the error text reads a
// corrupt repository or an unreadable object as an ordinary "no". The status
// is read by exitCode instead (gt-7j89ny).
func TestRefOutcomeChecksReadExitStatus(t *testing.T) {
	t.Parallel()

	const (
		branchCall = "show-ref --verify --quiet refs/heads/feat"
		refCall    = "show-ref --verify --quiet refs/heads/feat"
		remoteCall = "show-ref --verify --quiet refs/remotes/origin/main"
	)
	tests := []struct {
		name    string
		call    string
		reply   reply
		run     func(*Git) (bool, error)
		want    bool
		wantErr bool
	}{
		{
			name: "BranchExists found", call: branchCall, reply: ok(""),
			run: func(g *Git) (bool, error) { return g.BranchExists("feat") }, want: true,
		},
		{
			name: "BranchExists missing", call: branchCall, reply: fail(1, ""),
			run: func(g *Git) (bool, error) { return g.BranchExists("feat") },
		},
		{
			name: "BranchExists fatal repository", call: branchCall, reply: fail(128, "fatal: not a git repository"),
			run: func(g *Git) (bool, error) { return g.BranchExists("feat") }, wantErr: true,
		},
		{
			name: "BranchExists fatal with no stderr", call: branchCall, reply: fail(128, ""),
			run: func(g *Git) (bool, error) { return g.BranchExists("feat") }, wantErr: true,
		},
		{
			name: "RefExists found", call: refCall, reply: ok(""),
			run: func(g *Git) (bool, error) { return g.RefExists("refs/heads/feat") }, want: true,
		},
		{
			name: "RefExists missing", call: refCall, reply: fail(1, ""),
			run: func(g *Git) (bool, error) { return g.RefExists("refs/heads/feat") },
		},
		{
			name: "RefExists fatal repository", call: refCall, reply: fail(128, "fatal: not a git repository"),
			run: func(g *Git) (bool, error) { return g.RefExists("refs/heads/feat") }, wantErr: true,
		},
		{
			name: "RefExists fatal with no stderr", call: refCall, reply: fail(128, ""),
			run: func(g *Git) (bool, error) { return g.RefExists("refs/heads/feat") }, wantErr: true,
		},
		{
			name: "RemoteTrackingBranchExists found", call: remoteCall, reply: ok(""),
			run: func(g *Git) (bool, error) { return g.RemoteTrackingBranchExists("origin", "main") }, want: true,
		},
		{
			name: "RemoteTrackingBranchExists missing", call: remoteCall, reply: fail(1, ""),
			run: func(g *Git) (bool, error) { return g.RemoteTrackingBranchExists("origin", "main") },
		},
		{
			name: "RemoteTrackingBranchExists fatal repository", call: remoteCall, reply: fail(128, "fatal: not a git repository"),
			run: func(g *Git) (bool, error) { return g.RemoteTrackingBranchExists("origin", "main") }, wantErr: true,
		},
		{
			name: "RemoteTrackingBranchExists fatal with no stderr", call: remoteCall, reply: fail(128, ""),
			run: func(g *Git) (bool, error) { return g.RemoteTrackingBranchExists("origin", "main") }, wantErr: true,
		},
		{
			// IsEmpty's "no refs" answer is true whichever way git says it:
			// exit 1, or exit 0 with no output.
			name: "IsEmpty no refs by exit 1", call: "show-ref", reply: fail(1, ""),
			run: func(g *Git) (bool, error) { return g.IsEmpty() }, want: true,
		},
		{
			name: "IsEmpty no refs by empty output", call: "show-ref", reply: ok(""),
			run: func(g *Git) (bool, error) { return g.IsEmpty() }, want: true,
		},
		{
			name: "IsEmpty has refs", call: "show-ref", reply: ok("abc123 refs/heads/main\n"),
			run: func(g *Git) (bool, error) { return g.IsEmpty() },
		},
		{
			name: "IsEmpty fatal repository", call: "show-ref", reply: fail(128, "fatal: not a git repository"),
			run: func(g *Git) (bool, error) { return g.IsEmpty() }, wantErr: true,
		},
		{
			name: "IsEmpty fatal with no stderr", call: "show-ref", reply: fail(128, ""),
			run: func(g *Git) (bool, error) { return g.IsEmpty() }, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newTestGit(t, newScripted(map[string]reply{tt.call: tt.reply}))
			got, err := tt.run(g)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want an error: %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("answer = %v, want %v", got, tt.want)
			}
		})
	}
}

// scanExitStatusTextMatches lists the non-test Go files under dir, at any
// depth, that call strings.Contains with an argument holding "exit status":
// a string literal, or a named constant declared in the same file or in a
// non-test sibling file of the same directory. A comment that mentions the
// phrase is not a call and is not listed. A file is parsed only when its text
// holds the phrase or a Contains call, so a literal that spells the phrase
// with an escape, or a constant of another package, is outside this guard's
// reach; the guard exists to stop the plain text match from coming back.
//
// It returns the number of files it judged so a caller can tell an empty tree
// from a clean one, and an error for a walk or parse it could not complete:
// an unreadable tree is never reported as clean.
func scanExitStatusTextMatches(dir string) (matches []string, judged int, err error) {
	siblingConsts := map[string]map[string]string{}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Contains(src, []byte("exit status")) && !bytes.Contains(src, []byte("Contains")) {
			return nil
		}
		f, parseErr := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if parseErr != nil {
			return fmt.Errorf("parsing %s: %w", path, parseErr)
		}
		judged++
		pkgConsts, constErr := packageExitStatusConsts(filepath.Dir(path), siblingConsts)
		if constErr != nil {
			return constErr
		}
		consts := maps.Clone(pkgConsts)
		collectStringConsts(f, consts)
		if callsContainsExitStatus(f, consts) {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(matches)
	return matches, judged, nil
}

// packageExitStatusConsts returns the string constants declared in the
// non-test Go files directly under dir that hold the phrase, keyed by name.
// The result is cached per dir in cache. A sibling file that holds the phrase
// is parsed; one that does not cannot declare a constant with it.
func packageExitStatusConsts(dir string, cache map[string]map[string]string) (map[string]string, error) {
	if consts, ok := cache[dir]; ok {
		return consts, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	consts := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		if !bytes.Contains(src, []byte("exit status")) {
			continue
		}
		f, parseErr := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, parseErr)
		}
		collectStringConsts(f, consts)
	}
	cache[dir] = consts
	return consts, nil
}

// collectStringConsts adds to into every constant of f whose value is a
// string literal, at any scope. A name that a local variable shadows is still
// taken as the constant, which errs toward flagging.
func collectStringConsts(f *ast.File, into map[string]string) {
	ast.Inspect(f, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					break
				}
				if v, ok := stringLiteral(vs.Values[i]); ok {
					into[name.Name] = v
				}
			}
		}
		return true
	})
}

// stringLiteral returns the value of expr when it is a string literal.
func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// stringArg returns the string an argument holds: a name that consts maps, or
// a string literal.
func stringArg(arg ast.Expr, consts map[string]string) (string, bool) {
	if id, ok := arg.(*ast.Ident); ok {
		v, found := consts[id.Name]
		return v, found
	}
	return stringLiteral(arg)
}

// callsContainsExitStatus reports whether f calls the strings package's
// Contains with an argument holding "exit status": a string literal, or an
// identifier that consts maps to such a literal. An aliased import of strings
// and a dot import are both followed, so only renaming the method escapes it.
func callsContainsExitStatus(f *ast.File, consts map[string]string) bool {
	stringsName, dotImport := stringsImportName(f)
	if stringsName == "" && !dotImport {
		return false
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			pkg, isIdent := fun.X.(*ast.Ident)
			if !isIdent || fun.Sel.Name != "Contains" || pkg.Name != stringsName {
				return true
			}
		case *ast.Ident:
			if !dotImport || fun.Name != "Contains" {
				return true
			}
		default:
			return true
		}
		for _, arg := range call.Args {
			v, isString := stringArg(arg, consts)
			if isString && strings.Contains(v, "exit status") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// stringsImportName is the name f imports package strings under ("strings"
// unless aliased) and whether the import is a dot import.
func stringsImportName(f *ast.File) (name string, dot bool) {
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "strings" {
			continue
		}
		if imp.Name == nil {
			return "strings", false
		}
		if imp.Name.Name == "." {
			return "", true
		}
		return imp.Name.Name, false
	}
	return "", false
}

// TestNoExitStatusTextMatchUnderInternal is the guard: no non-test Go file
// under internal/ may decide a git outcome by searching an error's text for
// "exit status". "exit status 128" contains "exit status 1", so such a match
// takes a fatal error for an ordinary answer (gt-7j89ny). Read the status
// with exitCode instead.
func TestNoExitStatusTextMatchUnderInternal(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "internal")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("internal tree %s: %v", root, err)
	}
	matches, judged, err := scanExitStatusTextMatches(root)
	if err != nil {
		t.Fatalf("scanning %s: %v", root, err)
	}
	if judged == 0 {
		t.Fatalf("scanned %s and judged no file: the guard read nothing", root)
	}
	if len(matches) > 0 {
		t.Errorf("an error's text is matched against an \"exit status\" literal; read the exit code with exitCode instead: %v", matches)
	}
}

// TestExitStatusTextGuardDetectsMatch drives the guard's detector: a match is
// found through a plain and an aliased import, while a comment mentioning the
// phrase and a match in a test file are not flagged. Without this the guard
// could pass on an input it forbids and no one would know.
func TestExitStatusTextGuardDetectsMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	files := map[string]string{
		"bad.go":      "package x\n\nimport \"strings\"\n\nfunc banned(err error) bool {\n\treturn strings.Contains(err.Error(), \"exit status 1\")\n}\n",
		"aliased.go":  "package x\n\nimport s \"strings\"\n\nfunc banned(err error) bool {\n\treturn s.Contains(err.Error(), \"exit status 128\")\n}\n",
		"comment.go":  "package x\n\n// exit status 1 is git saying no; read the status, never the text.\nfunc fine() {}\n",
		"bad_test.go": "package x\n\nimport \"strings\"\n\nfunc check(err error) bool {\n\treturn strings.Contains(err.Error(), \"exit status 1\")\n}\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	matches, judged, err := scanExitStatusTextMatches(dir)
	if err != nil {
		t.Fatalf("scanning fixture: %v", err)
	}
	want := []string{filepath.Join(dir, "aliased.go"), filepath.Join(dir, "bad.go")}
	if !reflect.DeepEqual(matches, want) {
		t.Errorf("matches = %v, want %v", matches, want)
	}
	if judged != 3 {
		t.Errorf("judged %d files, want the three non-test files holding the phrase", judged)
	}
}

// TestExitStatusTextGuardResolvesNamedConstant drives the guard through a
// call whose argument is a named constant: the constant is resolved in the
// call's own file and in a sibling non-test file of its directory. A constant
// without the phrase, or one declared only in a test file, is not a match.
func TestExitStatusTextGuardResolvesNamedConstant(t *testing.T) {
	t.Parallel()

	const useSrc = "package x\n\nimport \"strings\"\n\nfunc banned(err error) bool {\n\treturn strings.Contains(err.Error(), exitText)\n}\n"
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name: "constant in the same file",
			files: map[string]string{
				"named.go": "package x\n\nimport \"strings\"\n\nconst exitText = \"exit status 1\"\n\nfunc banned(err error) bool {\n\treturn strings.Contains(err.Error(), exitText)\n}\n",
			},
			want: []string{"named.go"},
		},
		{
			name: "constant in a sibling file",
			files: map[string]string{
				"consts.go": "package x\n\nconst exitText = \"exit status 1\"\n",
				"use.go":    useSrc,
			},
			want: []string{"use.go"},
		},
		{
			name: "constant without the phrase",
			files: map[string]string{
				"consts.go": "package x\n\nconst exitText = \"exit\"\n",
				"use.go":    useSrc,
			},
		},
		{
			name: "constant declared only in a test file",
			files: map[string]string{
				"consts_test.go": "package x\n\nconst exitText = \"exit status 1\"\n",
				"use.go":         useSrc,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, src := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			matches, _, err := scanExitStatusTextMatches(dir)
			if err != nil {
				t.Fatalf("scanning fixture: %v", err)
			}
			var want []string
			for _, name := range tt.want {
				want = append(want, filepath.Join(dir, name))
			}
			if !reflect.DeepEqual(matches, want) {
				t.Errorf("matches = %v, want %v", matches, want)
			}
		})
	}
}

// TestExitStatusTextGuardFailsOnUnparsableSibling: a sibling file that holds
// the phrase but does not parse could declare the constant a call uses, so the
// guard cannot judge the call. It returns an error rather than a clean scan.
func TestExitStatusTextGuardFailsOnUnparsableSibling(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	files := map[string]string{
		// a_use.go sorts first, so the walk reaches the call before the broken
		// sibling and the constant lookup is what reports the parse error.
		"a_use.go":  "package x\n\nimport \"strings\"\n\nfunc banned(err error) bool {\n\treturn strings.Contains(err.Error(), exitText)\n}\n",
		"broken.go": "package x\n\nconst exitText = \"exit status 1\"\n\nfunc (\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	matches, _, err := scanExitStatusTextMatches(dir)
	if err == nil {
		t.Fatalf("matches = %v with no error, want the parse error for broken.go", matches)
	}
	if matches != nil {
		t.Errorf("matches = %v alongside the error, want none", matches)
	}
}

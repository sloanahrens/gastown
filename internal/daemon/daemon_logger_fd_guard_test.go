package daemon

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoTestLoggerWritesToTheProcessFD is the gt-uqjn structural guard: a
// logger attached to one of this package's long-lived structs in a test
// discards its output or buffers it, and never writes to the process fd. Those
// loggers stream subprocess output, and fd 2 goes straight to whatever captured
// the parent `go test ./...`, so fixture text logged through one reads as a
// real result for a package the tree has never contained (gt-tw45, gt-uqjn).
//
// It matches on the AST, not on text: the comments explaining this invariant
// quote log.New(os.Stderr, ...) in prose.
//
// The scan is syntactic, so a logger built into a local first (`l :=
// log.New(os.Stderr, ...)`) and then attached reaches it only if the test
// happens to name the local `logger`.
func TestNoTestLoggerWritesToTheProcessFD(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var violations []string
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		violations = append(violations, fdBackedLoggerFields(fset, name, file)...)
	}

	// A walk that found no tests would report a clean tree forever.
	if scanned == 0 {
		t.Fatalf("no _test.go files under %s to scan — the guard would pass vacuously", dir)
	}
	if len(violations) > 0 {
		t.Errorf("a logger field in a test writes to the process-wide stderr/stdout, leaking this "+
			"package's output into whatever captured `go test ./...` (gt-uqjn, gt-tw45).\n"+
			"Use discardLogger when the test never inspects the log, or log.New(&buf, ...) when it does:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestFDBackedLoggerFieldScanner pins the scanner on a planted violation: a
// walk that quietly stopped matching this shape would pass as a clean tree.
func TestFDBackedLoggerFieldScanner(t *testing.T) {
	const src = `package daemon

func f() {
	_ = &Daemon{logger: log.New(os.Stderr, "", 0)}
	_ = &Worker{logger: log.New(io.Discard, "", 0)}
	var d Daemon
	d.logger = log.Default()
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "planted.go", src, 0)
	if err != nil {
		t.Fatalf("parse planted source: %v", err)
	}

	got := strings.Join(fdBackedLoggerFields(fset, "planted.go", file), "\n")
	wanted := []string{"logger: log.New(os.Stderr, ...)", "logger: log.Default()"}
	for _, want := range wanted {
		if !strings.Contains(got, want) {
			t.Errorf("scanner missed %q; found:\n%s", want, got)
		}
	}
	if strings.Contains(got, "planted.go:5") {
		t.Errorf("scanner flagged the io.Discard logger; found:\n%s", got)
	}
}

// TestFDBackedLoggerDetector pins the expression matcher the scanner uses.
func TestFDBackedLoggerDetector(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`log.New(os.Stderr, "", 0)`, true},
		{`log.New(os.Stdout, "test: ", log.LstdFlags)`, true},
		{`log.Default()`, true}, // log.Default() writes to os.Stderr
		{`log.New(io.Discard, "", 0)`, false},
		{`log.New(&buf, "", 0)`, false},
		{`log.New(logBuf, "", 0)`, false},
		{`log.New(os.Stdin, "", 0)`, false}, // only the two captured streams count
		{`slog.New(slog.NewTextHandler(os.Stderr, nil))`, false},
	}
	for _, tc := range cases {
		expr, err := parser.ParseExpr(tc.src)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", tc.src, err)
		}
		if _, got := fdBackedLogger(expr); got != tc.want {
			t.Errorf("fdBackedLogger(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// fdBackedLoggerFields names each `logger` field set in file — as a struct
// literal key or as an assignment — to a logger that would reach the
// process-wide stderr or stdout, as "file:line: shape" for the failure message.
func fdBackedLoggerFields(fset *token.FileSet, name string, file *ast.File) []string {
	var found []string
	report := func(pos token.Pos, expr ast.Expr) {
		shape, ok := fdBackedLogger(expr)
		if !ok {
			return
		}
		found = append(found, fmt.Sprintf("%s:%d: logger: %s", name, fset.Position(pos).Line, shape))
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "logger" {
					report(kv.Pos(), kv.Value)
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "logger" || i >= len(node.Rhs) {
					continue
				}
				report(node.Rhs[i].Pos(), node.Rhs[i])
			}
		}
		return true
	})
	return found
}

// fdBackedLogger reports whether expr constructs a *log.Logger writing to the
// process-wide stderr or stdout, and renders the matched shape.
func fdBackedLogger(expr ast.Expr) (string, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "log" {
		return "", false
	}

	switch sel.Sel.Name {
	case "Default":
		return "log.Default()", true
	case "New":
		if len(call.Args) == 0 {
			return "", false
		}
		stream, ok := capturedStream(call.Args[0])
		if !ok {
			return "", false
		}
		return "log.New(" + stream + ", ...)", true
	}
	return "", false
}

// capturedStream matches the two process-wide streams a parent `go test ./...`
// invocation captures, os.Stderr and os.Stdout.
func capturedStream(expr ast.Expr) (string, bool) {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "os" {
		return "", false
	}
	switch sel.Sel.Name {
	case "Stderr", "Stdout":
		return "os." + sel.Sel.Name, true
	}
	return "", false
}

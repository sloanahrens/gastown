package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// helperSubcommands are the first arguments the image's helper dispatches
// (internal/polecatimage/program.go helperRuns). The daemon must never pass the
// helper another: the shipped runProgram sends an unknown first argument to a
// full plugin run, so an older image would not answer the ask (gt-gg9hv.41).
// node-prune joins at c2bf1bad (gt-kpc4tp); its Job runs the running pair's
// image, which descends from that commit.
var helperSubcommands = []string{
	"trim-cache", "trim-paths", "trim-list", "trim-list-write",
	"seed-clone", "seed-cache", "node-prune",
}

// helperArgsIn returns, for each list in src that names cachevol.HelperPath,
// the literal after it, or a "<...>" problem text when none follows.
func helperArgsIn(name string, src []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		return nil, err
	}
	var args []string
	ast.Walk(&cacheTrimWalker{visit: func(n ast.Node, ancestors []ast.Node) {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !cacheTrimIsHelperPath(sel) {
			return
		}
		elements := cacheTrimArgList(ancestors)
		i := slices.IndexFunc(elements, func(e ast.Expr) bool { return e == sel })
		if i < 0 || i+1 >= len(elements) {
			args = append(args, "<HelperPath has no argument after it>")
			return
		}
		lit, ok := elements[i+1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			args = append(args, "<not a string literal>")
			return
		}
		value, _ := strconv.Unquote(lit.Value)
		args = append(args, value)
	}}, file)
	return args, nil
}

// TestHelperSubcommandsAreDispatchedByTheImage reads the daemon's production
// files and requires the literal beside each cachevol.HelperPath to be a
// subcommand the image dispatches; a new subcommand fails here before an older
// image meets it.
func TestHelperSubcommandsAreDispatchedByTheImage(t *testing.T) {
	t.Parallel()
	scratch := map[string]string{
		"an unknown literal":   `package p; var _ = f(cachevol.HelperPath, "trim-everything")`,
		"a computed argument":  `package p; var _ = f(cachevol.HelperPath, verb)`,
		"HelperPath ends it":   `package p; var _ = f("x", cachevol.HelperPath)`,
		"a dispatched literal": `package p; var _ = f(cachevol.HelperPath, "node-prune")`,
	}
	for name, src := range scratch {
		args, err := helperArgsIn(name, []byte(src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		bad := len(args) != 1 || !slices.Contains(helperSubcommands, args[0])
		if bad != (name != "a dispatched literal") {
			t.Errorf("%s: scanner read %q, unknown=%v", name, args, bad)
		}
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	sites := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		args, err := helperArgsIn(name, src)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, arg := range args {
			sites++
			if !slices.Contains(helperSubcommands, arg) {
				t.Errorf("%s passes the helper %q, which the image does not dispatch", name, arg)
			}
		}
	}
	// Exactly the node-prune site (the trim-list site went with the old listing
	// route): one that moves or is added must be seen.
	if sites != 1 {
		t.Errorf("the scan found %d helper call sites in the daemon, want 1 (node-prune)", sites)
	}
}

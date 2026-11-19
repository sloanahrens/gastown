//go:build integration

package makegate

import (
	"fmt"
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

// dryRunTargets names the Makefile targets whose recipe line holds $(MAKE), so
// GNU make runs it for real even under -n (gt-digv4n, sweep f213fda4).
//
// Before changing either name, read the Makefile comment above ci-base-image
// and the presubmit recipe.
var dryRunTargets = []string{"presubmit", "ci-base-image"}

// dryRunFactory names the helpers that return a Make bound to a checkout. A
// call of the Make one returns is a dry run of the tree the helper was given.
var dryRunFactory = []string{"RealMake", "defaultsMake"}

// dryRunTargetCall is one refused dry run: the call's position, the helper or
// variable that ran it, and the target it named.
type dryRunTargetCall struct {
	file   string
	line   int
	caller string
	target string
}

func (c dryRunTargetCall) String() string {
	return fmt.Sprintf("%s:%d: %s names %q, whose recipe line holds $(MAKE) and so runs for real under make -n (gt-digv4n)",
		c.file, c.line, c.caller, c.target)
}

// dryRunPackageCalls returns every refused dry run in the *_test.go files of
// dir. A read or parse failure in any file is an error, and so is a directory
// with no test file: a guard that read nothing has not cleared the package.
func dryRunPackageCalls(dir string) ([]dryRunTargetCall, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, fmt.Errorf("list the test files in %s: %w", dir, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no *_test.go in %s: the guard read nothing and cannot clear the package", dir)
	}
	var calls []dryRunTargetCall
	for _, path := range paths {
		found, err := dryRunFileTargetCalls(path)
		if err != nil {
			return nil, err
		}
		calls = append(calls, found...)
	}
	return calls, nil
}

// dryRunFileTargetCalls reads one file and returns its refused dry runs. A read
// or parse failure is returned with the file's path: a guard that cannot see a
// file fails instead of passing.
func dryRunFileTargetCalls(path string) ([]dryRunTargetCall, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return dryRunTargetCalls(path, src)
}

// dryRunTargetCalls returns one entry per call in src that hands a target in
// dryRunTargets to dryTarget or to a Make from dryRunFactory. The Make may be
// the factory call itself (RealMake(root)("presubmit")), the second argument of
// dryTarget, or a variable assigned from a factory earlier in the file. Only a
// string literal argument counts. The scratch-checkout runners (runTarget, run,
// SeedMake) run the recipe for real in a scratch checkout, not on this tree,
// and are not matched.
func dryRunTargetCalls(file string, src []byte) ([]dryRunTargetCall, error) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	makes := makeVariables(parsed)
	var (
		calls []dryRunTargetCall
		bad   error
	)
	ast.Inspect(parsed, func(n ast.Node) bool {
		if bad != nil {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		caller := dryRunCaller(call, makes)
		if caller == "" {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			target, err := literalString(lit)
			if err != nil {
				bad = fmt.Errorf("%s: %w", fset.Position(lit.Pos()), err)
				return false
			}
			if slices.Contains(dryRunTargets, target) {
				calls = append(calls, dryRunTargetCall{
					file:   file,
					line:   fset.Position(call.Pos()).Line,
					caller: caller,
					target: target,
				})
			}
		}
		return true
	})
	if bad != nil {
		return nil, bad
	}
	return calls, nil
}

// literalString returns the value of a string literal. A literal the guard
// cannot decode is an error, not a target it does not name: a value it cannot
// read must not pass as one it has checked.
func literalString(lit *ast.BasicLit) (string, error) {
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", fmt.Errorf("unquote %s: %w", lit.Value, err)
	}
	return value, nil
}

// dryRunCaller names what ran call, or "" when nothing in this guard's scope
// did: dryTarget itself, a variable holding a Make from a factory, or a factory
// whose returned Make the call invokes.
func dryRunCaller(call *ast.CallExpr, makes map[string]bool) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		if fn.Name == "dryTarget" || makes[fn.Name] {
			return fn.Name
		}
	case *ast.CallExpr:
		if id, ok := fn.Fun.(*ast.Ident); ok && slices.Contains(dryRunFactory, id.Name) {
			return id.Name
		}
	}
	return ""
}

// makeVariables returns the identifiers file assigns a Make returned by a
// factory, so a later call of one is a call of that factory's Make.
func makeVariables(file *ast.File) map[string]bool {
	makes := map[string]bool{}
	fromFactory := func(expr ast.Expr) bool {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := call.Fun.(*ast.Ident)
		return ok && slices.Contains(dryRunFactory, id.Name)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range s.Rhs {
				if i < len(s.Lhs) && fromFactory(rhs) {
					if id, ok := s.Lhs[i].(*ast.Ident); ok {
						makes[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for i, value := range s.Values {
				if i < len(s.Names) && fromFactory(value) {
					makes[s.Names[i].Name] = true
				}
			}
		}
		return true
	})
	return makes
}

// dryRunAlarms returns one message per refused dry run, each naming the file,
// line, caller and target. The package case in TestIntegrationDryRunCallers
// reports exactly these.
func dryRunAlarms(calls []dryRunTargetCall) []string {
	alarms := make([]string, 0, len(calls))
	for _, call := range calls {
		alarms = append(alarms, call.String())
	}
	return alarms
}

// packageTestDir is the directory holding this file, read from the compiled
// path so the guard checks the package it was built in rather than the working
// directory.
func packageTestDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(here)
}

// TestIntegrationDryRunCallers fails any test in this package that dry-runs
// presubmit or ci-base-image on the real tree, where the recipe runs for real
// (gt-digv4n, sweep f213fda4). It reads the package's sources, so it starts no
// make process.
//
// Mutation: add `dryTarget(t, defaultsMake(t, repoRoot(t)), "presubmit")` to a
// test file, and the package case fails naming that file and line.
func TestIntegrationDryRunCallers(t *testing.T) {
	t.Run("the package's test files", func(t *testing.T) {
		calls, err := dryRunPackageCalls(packageTestDir(t))
		if err != nil {
			t.Fatalf("%v", err)
		}
		for _, alarm := range dryRunAlarms(calls) {
			t.Error(alarm)
		}
	})

	t.Run("a scratch file's dry run reaches the alarm", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "scratch_test.go")
		src := "package makegate\n\nfunc TestX(t *testing.T) {\n\tdryTarget(t, defaultsMake(t, root), \"presubmit\")\n}\n"
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		calls, err := dryRunPackageCalls(dir)
		if err != nil {
			t.Fatalf("dryRunPackageCalls(%s): %v", dir, err)
		}
		alarms := dryRunAlarms(calls)
		if len(alarms) != 1 {
			t.Fatalf("dryRunAlarms = %q, want one alarm for %s", alarms, path)
		}
		if !strings.Contains(alarms[0], path+":4:") {
			t.Fatalf("the alarm %q does not name %s:4: the call's line", alarms[0], path)
		}
	})

	t.Run("source snippets", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			src  string
			want []string // "<caller> <target>", one per finding
		}{
			{
				name: "dryTarget names presubmit",
				src:  "func TestX(t *testing.T) {\n\tdryTarget(t, defaultsMake(t, root), \"presubmit\")\n}\n",
				want: []string{"dryTarget presubmit"},
			},
			{
				name: "dryTarget names ci-base-image",
				src:  "func TestX(t *testing.T) {\n\tdryTarget(t, RealMake(root), \"ci-base-image\")\n}\n",
				want: []string{"dryTarget ci-base-image"},
			},
			{
				name: "an invoked defaultsMake call names ci-base-image",
				src:  "func TestX(t *testing.T) {\n\tout, err := defaultsMake(t, root)(\"ci-base-image\")\n\t_, _ = out, err\n}\n",
				want: []string{"defaultsMake ci-base-image"},
			},
			{
				name: "a RealMake variable names ci-base-image",
				src:  "func TestX(t *testing.T) {\n\tm := RealMake(root)\n\tout, err := m(\"ci-base-image\")\n\t_, _ = out, err\n}\n",
				want: []string{"m ci-base-image"},
			},
			{
				name: "a raw-string target is read, not matched by its quotes",
				src:  "func TestX(t *testing.T) {\n\tdryTarget(t, defaultsMake(t, root), `presubmit`)\n}\n",
				want: []string{"dryTarget presubmit"},
			},
			{
				name: "a single-command target is left alone",
				src:  "func TestX(t *testing.T) {\n\tdryTarget(t, polecatImageMake(t, root), \"polecat-image\")\n}\n",
				want: nil,
			},
			{
				name: "the scratch-checkout runners are left alone",
				src:  "func TestX(t *testing.T) {\n\ts := newGateScratch(t)\n\ts.runTarget(\"presubmit\", nil, \"X=1\")\n\ts.SeedMake(\"build\", \"presubmit\")\n\tp.run(t, nil, \"presubmit\")\n}\n",
				want: nil,
			},
			{
				// The guard reads string literals only, so a target held in a
				// variable is not seen. Failing closed on a non-literal target
				// is gt-xusszl.
				name: "known limit: a target named through a variable is not seen",
				src:  "func TestX(t *testing.T) {\n\ttarget := \"presubmit\"\n\tdryTarget(t, defaultsMake(t, root), target)\n}\n",
				want: nil,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// A test file needs its package clause; the body under test
				// is what the row carries.
				calls, err := dryRunTargetCalls("snippet_test.go", []byte("package makegate\n\n"+tc.src))
				if err != nil {
					t.Fatalf("dryRunTargetCalls: %v", err)
				}
				var got []string
				for _, call := range calls {
					got = append(got, call.caller+" "+call.target)
				}
				if !slices.Equal(got, tc.want) {
					t.Fatalf("findings = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("a string literal that cannot be decoded fails", func(t *testing.T) {
		// The Go parser rejects a bad escape before the walk sees the literal,
		// so the literal is built here rather than parsed.
		bad := &ast.BasicLit{Kind: token.STRING, Value: "\"\\uD800\""}
		if _, err := literalString(bad); err == nil {
			t.Fatal("literalString accepted a literal strconv.Unquote rejects: an undecodable target would pass as clean")
		}
	})

	t.Run("a file that cannot be read fails naming it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing_test.go")
		_, err := dryRunFileTargetCalls(path)
		if err == nil {
			t.Fatalf("dryRunFileTargetCalls(%s) returned no error: an unreadable file passed as clean", path)
		}
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("the error does not name %s: %v", path, err)
		}
	})

	t.Run("a file that cannot be parsed fails naming it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "broken_test.go")
		if err := os.WriteFile(path, []byte("func TestX(t *testing.T) {\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		_, err := dryRunFileTargetCalls(path)
		if err == nil {
			t.Fatalf("dryRunFileTargetCalls(%s) returned no error: an unparseable file passed as clean", path)
		}
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("the error does not name %s: %v", path, err)
		}
	})

	t.Run("a directory with no test file fails", func(t *testing.T) {
		if _, err := dryRunPackageCalls(t.TempDir()); err == nil {
			t.Fatal("dryRunPackageCalls with no test file returned no error: an empty directory passed as clean")
		}
	})
}

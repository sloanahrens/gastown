package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// directiveDeployRecorder stands in for config.DeployCentralDirectives: it
// records each town it was asked to deploy into and returns the canned result.
type directiveDeployRecorder struct {
	towns   []string
	written []string
	err     error
}

func (r *directiveDeployRecorder) deploy(townRoot string) ([]string, error) {
	r.towns = append(r.towns, townRoot)
	return r.written, r.err
}

// installWithDirectiveDeploy runs an install with no beads and the given
// directive deploy, and returns what the install printed for the directives.
func installWithDirectiveDeploy(t *testing.T, hqPath string, rec *directiveDeployRecorder) (string, error) {
	t.Helper()
	var warn bytes.Buffer
	opts := installOptions{
		noBeads: true,
		warn:    &warn,
		steps:   installSteps{deployDirectives: rec.deploy},
	}
	err := runInstallWith(opts, []string{hqPath})
	return warn.String(), err
}

// A deploy that fails is a warning the install's writer carries, naming the
// error, while the install itself succeeds (gt-3lvcjz.13).
func TestInstallDirectivesDeployFailureWarnsThroughTheWriter(t *testing.T) {
	t.Parallel()
	hqPath := filepath.Join(t.TempDir(), "hq")
	if err := os.MkdirAll(filepath.Join(hqPath, "gastown", ".repo.git"), 0o755); err != nil {
		t.Fatalf("create the gastown bare repo: %v", err)
	}
	failure := errors.New("commit 1234567 is not in the bare repo")
	rec := &directiveDeployRecorder{err: failure}

	out, err := installWithDirectiveDeploy(t, hqPath, rec)
	if err != nil {
		t.Fatalf("gt install with a failed directive deploy = %v, want the install to succeed", err)
	}
	if !strings.Contains(out, "Could not deploy central directives") {
		t.Errorf("warning = %q, want it to say the deploy failed", out)
	}
	if !strings.Contains(out, failure.Error()) {
		t.Errorf("warning = %q, want it to name the error %q", out, failure)
	}
}

// A gastown rig path whose stat fails with an error other than "does not exist"
// is a warning naming that error, and the deploy is not attempted.
func TestInstallDirectivesStatErrorWarnsNamingIt(t *testing.T) {
	t.Parallel()
	hqPath := filepath.Join(t.TempDir(), "hq")
	if err := os.MkdirAll(hqPath, 0o755); err != nil {
		t.Fatalf("create hq: %v", err)
	}
	// A regular file where the rig directory should be makes the stat of the
	// bare repo under it fail with ENOTDIR, which is not "does not exist".
	if err := os.WriteFile(filepath.Join(hqPath, "gastown"), []byte("not a rig"), 0o644); err != nil {
		t.Fatalf("write the blocking file: %v", err)
	}
	_, statErr := os.Stat(filepath.Join(hqPath, "gastown", ".repo.git"))
	if statErr == nil || errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stat under a regular file = %v, want an error other than not-exist", statErr)
	}
	rec := &directiveDeployRecorder{}

	out, err := installWithDirectiveDeploy(t, hqPath, rec)
	if err != nil {
		t.Fatalf("gt install = %v, want the install to succeed", err)
	}
	if !strings.Contains(out, "Could not check the gastown rig") || !strings.Contains(out, statErr.Error()) {
		t.Errorf("warning = %q, want it to name the stat error %q", out, statErr)
	}
	if len(rec.towns) != 0 {
		t.Errorf("deploy ran %d time(s) past a failed rig check, want none", len(rec.towns))
	}
}

// A town with no gastown rig yet skips the deploy with one line.
func TestInstallDirectivesNoRigSkipsWithOneLine(t *testing.T) {
	t.Parallel()
	hqPath := filepath.Join(t.TempDir(), "hq")
	rec := &directiveDeployRecorder{}

	out, err := installWithDirectiveDeploy(t, hqPath, rec)
	if err != nil {
		t.Fatalf("gt install = %v, want the install to succeed", err)
	}
	if len(rec.towns) != 0 {
		t.Errorf("deploy ran %d time(s) for a town with no gastown rig, want none", len(rec.towns))
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "central directives") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "no gastown rig yet") {
		t.Errorf("directive lines = %q, want exactly one saying there is no gastown rig yet", lines)
	}
}

// With a gastown rig present, the install deploys into its own town and reports
// how many files were written.
func TestInstallDirectivesDeploysIntoTheTownWhenTheRigExists(t *testing.T) {
	t.Parallel()
	hqPath := filepath.Join(t.TempDir(), "hq")
	if err := os.MkdirAll(filepath.Join(hqPath, "gastown", ".repo.git"), 0o755); err != nil {
		t.Fatalf("create the gastown bare repo: %v", err)
	}
	rec := &directiveDeployRecorder{written: []string{"directives/a.md"}}

	out, err := installWithDirectiveDeploy(t, hqPath, rec)
	if err != nil {
		t.Fatalf("gt install = %v, want the install to succeed", err)
	}
	if len(rec.towns) != 1 || rec.towns[0] != hqPath {
		t.Errorf("deploy towns = %q, want [%q]", rec.towns, hqPath)
	}
	if !strings.Contains(out, "1 written") {
		t.Errorf("output = %q, want it to report the one file written", out)
	}
}

// The default wiring passes version.BuildCommit() as the commit argument of
// config.DeployCentralDirectives. config cannot import version, so the call
// site is where the build commit is supplied; this reads install.go to hold it
// there (gt-3lvcjz.13).
func TestInstallDirectivesDefaultWiringPassesBuildCommit(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "install.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse install.go: %v", err)
	}
	var calls []*ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if ok && isSelector(call.Fun, "config", "DeployCentralDirectives") {
			calls = append(calls, call)
		}
		return true
	})
	if len(calls) != 1 {
		t.Fatalf("config.DeployCentralDirectives call sites in install.go = %d, want 1", len(calls))
	}
	args := calls[0].Args
	if len(args) != 3 {
		t.Fatalf("DeployCentralDirectives takes %d arguments at the call site, want 3", len(args))
	}
	commitArg, ok := args[1].(*ast.CallExpr)
	if !ok || !isSelector(commitArg.Fun, "version", "BuildCommit") || len(commitArg.Args) != 0 {
		t.Errorf("commit argument at %s = %s, want version.BuildCommit()",
			fset.Position(args[1].Pos()), exprText(fset, args[1]))
	}
	if !isSelector(args[2], "version", "BuildCommit") {
		t.Errorf("build commit argument at %s = %s, want version.BuildCommit",
			fset.Position(args[2].Pos()), exprText(fset, args[2]))
	}
}

// isSelector reports whether expr is the selector pkg.name.
func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

func exprText(fset *token.FileSet, expr ast.Expr) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, expr); err != nil {
		return fmt.Sprintf("<unprintable: %v>", err)
	}
	return b.String()
}

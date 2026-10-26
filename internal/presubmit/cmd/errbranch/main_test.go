package main

import (
	"bytes"
	"errors"
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/presubmit"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// TestReportNamesEachFindingAndFailsOnIt: a run with findings prints each one and returns an error;
// a run with none returns nil. Either way the counts of what was judged are printed (gt-b9j3o.5).
func TestReportNamesEachFindingAndFailsOnIt(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	res := presubmit.Result{Added: 2, Judged: 1, Quoted: 0, Findings: []presubmit.Finding{{Path: "a/x.go", Line: 9, Text: "return err"}}}
	if err := report(&out, res); err == nil {
		t.Error("a finding returned nil, so the presubmit would pass")
	}
	for _, want := range []string{"judged 1 of 2", "a/x.go:9: added error branch no test drives: return err"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report = %q, want it to contain %q", out.String(), want)
		}
	}

	out.Reset()
	if err := report(&out, presubmit.Result{Added: 0}); err != nil {
		t.Errorf("a clean branch returned %v", err)
	}
	if !strings.Contains(out.String(), "judged 0 of 0") {
		t.Errorf("a clean branch printed %q, want the judged count", out.String())
	}
}

// TestModulePathRefusesAFailedListing: when go list -m fails, the check refuses rather than
// judging against an empty module path (gt-b9j3o.5).
func TestModulePathRefusesAFailedListing(t *testing.T) {
	t.Parallel()
	boom := errors.New("no go.mod")
	if got, err := modulePath(func() ([]byte, error) { return nil, boom }); !errors.Is(err, boom) || got != "" {
		t.Errorf("modulePath = %q, %v; want the listing's error and no module", got, err)
	}
	if got, err := modulePath(func() ([]byte, error) { return []byte("github.com/x/y\n"), nil }); err != nil || got != "github.com/x/y" {
		t.Errorf("modulePath = %q, %v; want github.com/x/y", got, err)
	}
}

// TestReportFailsWhenNothingWasJudged: added error returns the profile names no file of are a
// check that judged nothing, which must fail, not read as a clean branch (gt-b9j3o.5).
func TestReportFailsWhenNothingWasJudged(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := report(&out, presubmit.Result{Added: 3, Judged: 0}); err == nil {
		t.Error("three added error returns with none judged passed the check")
	}
	if err := report(&out, presubmit.Result{Added: 0, Judged: 0}); err != nil {
		t.Errorf("a branch with no added error returns failed: %v", err)
	}
}

// TestReportFailsAnUnjudgedReturn: an added error return the profile does not name fails the report
// and is printed by name, even beside judged returns (gt-b9j3o.5, overseer ruling).
func TestReportFailsAnUnjudgedReturn(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	res := presubmit.Result{Added: 2, Judged: 1, Unjudged: []presubmit.Finding{{Path: "root.go", Line: 3, Text: "return err"}}}
	if err := report(&out, res); err == nil {
		t.Error("an unjudged return passed the report")
	}
	if !strings.Contains(out.String(), "root.go:3:") {
		t.Errorf("report = %q, want the unjudged return named", out.String())
	}
}

// TestNestedModuleIn: a file under a directory with its own go.mod is nested; a file in the root
// module is not (ACCEPTED-RISK (a) on gt-b9j3o.5). The check reads a temp tree, no process.
func TestNestedModuleIn(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tracker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracker", "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := nestedModuleIn(root)
	if !nested("tracker/store/a.go") {
		t.Error("a file under a nested module was not nested")
	}
	if nested("internal/a.go") {
		t.Error("a root-module file was called nested")
	}
}

// TestBuildExcludedInFollowsTheTargetPlatform: a file the target platform's constraints exclude is
// reported as excluded, a file it builds is not, and the host's own GOOS does not matter: the
// context is given (gt-b9j3o.5).
func TestBuildExcludedInFollowsTheTargetPlatform(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, body := range map[string]string{
		"a_darwin.go": "package a\n",
		"b_linux.go":  "package a\n",
		"c.go":        "//go:build ignore\n\npackage a\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	linux := &build.Context{GOOS: "linux", GOARCH: "amd64", Compiler: "gc"}
	excluded := buildExcludedIn(root, linux)
	if !excluded("a_darwin.go") {
		t.Error("a darwin-only file was not excluded on linux")
	}
	if excluded("b_linux.go") {
		t.Error("a linux file was excluded on linux")
	}
	if !excluded("c.go") {
		t.Error("a file behind //go:build ignore was not excluded")
	}
}

// TestReportDoesNotFailExcluded: excluded returns are printed as excluded and do not fail the report.
func TestReportDoesNotFailExcluded(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	res := presubmit.Result{Added: 1, Judged: 1, Excluded: []presubmit.Finding{{Path: "a_darwin.go", Line: 3, Text: "return err"}}}
	if err := report(&out, res); err != nil {
		t.Errorf("an excluded return failed the report: %v", err)
	}
	if !strings.Contains(out.String(), "excluded by build constraints") {
		t.Errorf("report = %q, want the excluded return named as excluded", out.String())
	}
}

// TestBuildExcludedInFailsClosedOnAnUnreadableFile: a file the build-constraint match cannot read is
// not treated as excluded, so an added error return in it stays a failure: the error branch fails
// closed (gt-b9j3o.5; the survivor the overseer accepted, closed here).
func TestBuildExcludedInFailsClosedOnAnUnreadableFile(t *testing.T) {
	t.Parallel()
	linux := &build.Context{GOOS: "linux", GOARCH: "amd64", Compiler: "gc"}
	if buildExcludedIn(t.TempDir(), linux)("missing.go") {
		t.Error("an unreadable file was treated as excluded: it must fail closed")
	}
}

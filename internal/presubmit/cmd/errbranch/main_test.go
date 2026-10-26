package main

import (
	"bytes"
	"errors"
	"os"
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

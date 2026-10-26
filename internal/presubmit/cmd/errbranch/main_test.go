package main

import (
	"bytes"
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

package scorecard

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/testsel"
)

var updateTestselGolden = flag.Bool("update-testsel-golden", false, "rewrite testdata/testsel.golden")

// testselFixtureLog is the test-selection log the golden report reads. Line A
// has a failed package the battery would skip at 0.5, so it is a MISS there.
// Line B has no gate results, so it is logged and never labeled. Line C is from
// another question set and is skipped whole. Line D has a package it would skip
// at 0.3 that the gate failed, and one it would skip with no gate result.
var testselFixtureLog = []string{
	`{"ts":"2026-10-18T00:00:00Z","bead":"gt-a","rig":"gastown","head":"h1","changed":["internal/land"],"always_run":["internal/land"],"asked":["internal/cmd","internal/daemon","internal/land"],"noul":{"internal/cmd":0.1,"internal/daemon":0.9,"internal/land":0.4},"would_skip":{},"miss":{},"ungraded":{},"gate_packages":3,"gate":{"internal/cmd":true,"internal/daemon":true,"internal/land":false},"question_version":1,"model":"m1"}`,
	`{"ts":"2026-10-19T00:00:00Z","bead":"gt-b","rig":"gastown","head":"h2","changed":["internal/jev"],"always_run":["internal/jev"],"asked":["internal/cmd","internal/daemon"],"noul":{"internal/cmd":0.6,"internal/daemon":0.05},"would_skip":{},"miss":{},"ungraded":{},"gate_packages":0,"question_version":1,"model":"m1"}`,
	`{"ts":"2026-10-19T12:00:00Z","bead":"gt-c","rig":"gastown","head":"h3","asked":["internal/cmd"],"noul":{"internal/cmd":0.2},"gate":{"internal/cmd":true},"question_version":0,"model":"m0"}`,
	`{"ts":"2026-10-20T00:00:00Z","bead":"gt-d","rig":"gastown","head":"h4","changed":["internal/cmd"],"always_run":["internal/cmd"],"asked":["internal/land","internal/cmd"],"noul":{"internal/cmd":0.2,"internal/land":0.1},"gate_packages":1,"gate":{"internal/cmd":false},"question_version":1,"model":"m1"}`,
}

// TestTestselScorecardPrintsPerPackageBlockAndSummary is the testsel battery's
// golden test. It opens the adapter the way gt jev scorecard does, reads the
// fixture log, and checks the per-package block and the per-threshold summary.
func TestTestselScorecardPrintsPerPackageBlockAndSummary(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logPath := testsel.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(testselFixtureLog, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(TestselBattery, town)
	if err != nil {
		t.Fatal(err)
	}

	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 7 || rep.LinesSkipped != 1 {
		t.Fatalf("items %d skipped %d, want 7 and 1", rep.Items, rep.LinesSkipped)
	}

	var out strings.Builder
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	goldenPath := filepath.Join("testdata", "testsel.golden")
	if *updateTestselGolden {
		if err := os.MkdirAll("testdata", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-testsel-golden):\n%s\nwant:\n%s", goldenPath, got, want)
	}
}

// TestTestselParseLinesOneItemPerAskedPackage checks that a line becomes one
// item per package Jev answered, in directory order, carrying the gate result
// the package has, or none when the gate reported no result for it.
func TestTestselParseLinesOneItemPerAskedPackage(t *testing.T) {
	t.Parallel()
	a := newTestselAdapter("unused")
	items, err := a.ParseLines([]byte(testselFixtureLog[3]))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items %d, want 2", len(items))
	}
	if items[0].Refs[refGate] != gateFailed || items[1].Refs[refGate] != "" {
		t.Errorf("gate refs %q and %q, want failed for internal/cmd and none for internal/land",
			items[0].Refs[refGate], items[1].Refs[refGate])
	}
	if items[0].Answers[testselAffects].Noul == nil || *items[0].Answers[testselAffects].Noul != 0.2 {
		t.Errorf("internal/cmd noul %v, want 0.2", items[0].Answers[testselAffects].Noul)
	}
}

// TestTestselLabelFromGateResult checks the label each package takes from the
// gate: failed is affected, passed is not, and no gate result is no label.
func TestTestselLabelFromGateResult(t *testing.T) {
	t.Parallel()
	a := newTestselAdapter("unused")
	cases := []struct {
		name    string
		line    string
		pkg     int
		labeled bool
		affects bool
	}{
		{name: "passed package is not affected", line: testselFixtureLog[0], pkg: 0, labeled: true, affects: false},
		{name: "failed package is affected", line: testselFixtureLog[0], pkg: 2, labeled: true, affects: true},
		{name: "a line without gate results gives no label", line: testselFixtureLog[1], pkg: 0, labeled: false},
		{name: "a package with no gate result gives no label", line: testselFixtureLog[3], pkg: 1, labeled: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			items, err := a.ParseLines([]byte(tc.line))
			if err != nil {
				t.Fatal(err)
			}
			lab, err := a.Label(context.Background(), items[tc.pkg])
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled != tc.labeled {
				t.Fatalf("labeled %v, want %v", lab.Labeled, tc.labeled)
			}
			if tc.labeled && lab.Labels[testselAffects] != tc.affects {
				t.Errorf("affects label %v, want %v", lab.Labels[testselAffects], tc.affects)
			}
		})
	}
}

// TestTestselSkipsALineFromAnotherQuestionSet checks that a line the battery
// logged under a different question set is skipped whole, not misread.
func TestTestselSkipsALineFromAnotherQuestionSet(t *testing.T) {
	t.Parallel()
	a := newTestselAdapter("unused")
	if _, err := a.ParseLines([]byte(testselFixtureLog[2])); err == nil {
		t.Fatal("a line from question set 0 parsed, want an error")
	}
}

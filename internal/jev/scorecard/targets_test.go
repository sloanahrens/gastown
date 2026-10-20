package scorecard

import (
	"context"
	"errors"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/targets"
)

var updateTargetsGolden = flag.Bool("update-targets-golden", false, "rewrite testdata/targets.golden")

// fakeRunLedger returns scripted run beads and records every read.
type fakeRunLedger struct {
	byBead map[string]runBead
	err    error
	reads  []string
}

func (f *fakeRunLedger) Read(bead string) (runBead, error) {
	f.reads = append(f.reads, bead)
	if f.err != nil {
		return runBead{}, f.err
	}
	return f.byBead[bead], nil
}

func targetsItem(run, path string) Item {
	return Item{
		ID:   path,
		Time: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC),
		Refs: map[string]string{refRun: run},
	}
}

func TestTargetsParseReadsTheLogLine(t *testing.T) {
	t.Parallel()
	a := &targetsAdapter{}
	raw := []byte(`{"ts":"2026-10-19T09:00:00Z","run":"gt-run1","path":"internal/a/a.go","tier":1,"risk_score":9.5,"bytes":1200,"model":"jev-1.13.0","answers":{"likely_defect":{"noul":0.9},"review_priority":{"score":2,"confidence":0.7}},"question_version":1}`)
	it, err := a.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if it.ID != "internal/a/a.go" || it.Model != "jev-1.13.0" {
		t.Errorf("item id %q model %q, want the path and jev-1.13.0", it.ID, it.Model)
	}
	if got := it.Refs[refRun]; got != "gt-run1" {
		t.Errorf("run = %q, want gt-run1", got)
	}
	if q := it.Answers[targets.QLikelyDefect]; q.Noul == nil || *q.Noul != 0.9 {
		t.Errorf("likely_defect answer = %+v, want noul 0.9", q)
	}
	if q := it.Answers[targets.QReviewPriority]; q.Score == nil || *q.Score != 2 {
		t.Errorf("review_priority answer = %+v, want score 2", q)
	}
}

func TestTargetsParseRejectsIncompleteAndOtherQuestionSetLines(t *testing.T) {
	t.Parallel()
	a := &targetsAdapter{}
	for _, raw := range []string{
		`not json`,
		`{"ts":"2026-10-19T09:00:00Z","path":"internal/a/a.go","question_version":1}`,
		`{"ts":"2026-10-19T09:00:00Z","run":"gt-run1","question_version":1}`,
		`{"run":"gt-run1","path":"internal/a/a.go","question_version":1}`,
		`{"ts":"2026-10-19T09:00:00Z","run":"gt-run1","path":"internal/a/a.go"}`,
		`{"ts":"2026-10-19T09:00:00Z","run":"gt-run1","path":"internal/a/a.go","question_version":99}`,
	} {
		if _, err := a.Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%s) succeeded, want an error", raw)
		}
	}
}

// TestTargetsLedgerLineFile table-tests the three ledger line shapes, and one
// malformed line of each shape, which the parser skips.
func TestTargetsLedgerLineFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		file string
		ok   bool
	}{
		{
			name: "a finding line names the fingerprint's file",
			line: "finding: internal/c/c.go:Check:wrong-branch gt-f1",
			file: "internal/c/c.go",
			ok:   true,
		},
		{
			name: "a digest-only finding still names its file",
			line: "finding: internal/c/c.go:Check:wrong-branch digest-only",
			file: "internal/c/c.go",
			ok:   true,
		},
		{
			name: "a queued line names the fingerprint's file",
			line: "queued: internal/a/a.go:Do:missing-guard P2 internal/a/a.go:12 the guard is missing",
			file: "internal/a/a.go",
			ok:   true,
		},
		{
			name: "a p3 line names the fingerprint's file",
			line: "p3: 2026-10-18 internal/b/b.go:Fetch:doc-drift internal/b/b.go:9 the comment lies",
			file: "internal/b/b.go",
			ok:   true,
		},
		{
			name: "a finding line without a bead id is malformed",
			line: "finding: internal/c/c.go:Check:wrong-branch",
		},
		{
			name: "a queued line with an unknown severity is malformed",
			line: "queued: internal/a/a.go:Do:missing-guard P4 internal/a/a.go:12 the guard is missing",
		},
		{
			name: "a queued line with no claim is malformed",
			line: "queued: internal/a/a.go:Do:missing-guard P2",
		},
		{
			name: "a p3 line with a bad date is malformed",
			line: "p3: 2026-13-45 internal/b/b.go:Fetch:doc-drift internal/b/b.go:9 the comment lies",
		},
		{
			name: "a p3 line without a citation is malformed",
			line: "p3: 2026-10-18 internal/b/b.go:Fetch:doc-drift claim",
		},
		{
			name: "a two-token fingerprint is malformed",
			line: "p3: 2026-10-18 internal/b/b.go:doc-drift internal/b/b.go:9 the comment lies",
		},
		{
			name: "another ledger line names no file",
			line: "cursor: 2026-10-19T08:00:00Z internal/d/d.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file, ok := ledgerLineFile(tc.line)
			if ok != tc.ok || file != tc.file {
				t.Errorf("ledgerLineFile(%q) = %q, %v, want %q, %v", tc.line, file, ok, tc.file, tc.ok)
			}
		})
	}
}

func TestTargetsLabelReadsTheRunLedger(t *testing.T) {
	t.Parallel()
	notes := strings.Join([]string{
		"cursor: 2026-10-19T08:00:00Z internal/d/d.go",
		"reviewed: 2026-10-19 internal/a/a.go",
		"finding: internal/c/c.go:Check:wrong-branch gt-f1",
		"queued: internal/e/e.go:Run:missing-guard P2 internal/e/e.go:14 the guard is missing",
		"p3: 2026-10-18 internal/b/b.go:Fetch:doc-drift internal/b/b.go:9 the comment lies",
		"queued: internal/a/a.go:Do:bad P4 internal/a/a.go:1 the severity is not one",
	}, "\n")
	closed := runBead{Found: true, Status: "closed", Notes: notes}
	cases := []struct {
		name    string
		bead    runBead
		path    string
		labeled bool
		found   bool
	}{
		{name: "a finding line names the file", bead: closed, path: "internal/c/c.go", labeled: true, found: true},
		{name: "a queued line names the file", bead: closed, path: "internal/e/e.go", labeled: true, found: true},
		{name: "a p3 line names the file", bead: closed, path: "internal/b/b.go", labeled: true, found: true},
		{name: "a file no line names is clean", bead: closed, path: "internal/d/d.go", labeled: true},
		{name: "a file only a malformed line names is clean", bead: closed, path: "internal/a/a.go", labeled: true},
		{
			name: "an open run labels nothing",
			bead: runBead{Found: true, Status: "open", Notes: notes},
			path: "internal/c/c.go",
		},
		{
			name: "a hooked run labels nothing",
			bead: runBead{Found: true, Status: "hooked", Notes: notes},
			path: "internal/c/c.go",
		},
		{name: "a run bead no longer in the store labels nothing", bead: runBead{}, path: "internal/c/c.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &targetsAdapter{runs: &fakeRunLedger{byBead: map[string]runBead{"gt-run1": tc.bead}}}
			lab, err := a.Label(context.Background(), targetsItem("gt-run1", tc.path))
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled != tc.labeled {
				t.Fatalf("labeled = %v, want %v", lab.Labeled, tc.labeled)
			}
			if !tc.labeled {
				if lab.Labels != nil || lab.Rule != "" {
					t.Errorf("unlabeled item returned labels %v rule %q, want none", lab.Labels, lab.Rule)
				}
				return
			}
			want := map[QuestionID]bool{
				targets.QLikelyDefect:   tc.found,
				targets.QReviewPriority: tc.found,
			}
			if !maps.Equal(lab.Labels, want) {
				t.Errorf("labels = %v, want %v", lab.Labels, want)
			}
			wantRule := ruleTargetsClean
			if tc.found {
				wantRule = ruleTargetsLedger
			}
			if lab.Rule != wantRule {
				t.Errorf("rule = %q, want %q", lab.Rule, wantRule)
			}
		})
	}
}

func TestTargetsLabelReadsEachRunOnce(t *testing.T) {
	t.Parallel()
	ledger := &fakeRunLedger{byBead: map[string]runBead{
		"gt-run1": {Found: true, Status: "closed"},
	}}
	a := &targetsAdapter{runs: ledger}
	for _, path := range []string{"internal/a/a.go", "internal/b/b.go"} {
		if _, err := a.Label(context.Background(), targetsItem("gt-run1", path)); err != nil {
			t.Fatal(err)
		}
	}
	if len(ledger.reads) != 1 || ledger.reads[0] != "gt-run1" {
		t.Errorf("reads = %v, want one read of gt-run1", ledger.reads)
	}
}

func TestTargetsLabelFailsWhenTheRunCannotBeRead(t *testing.T) {
	t.Parallel()
	a := &targetsAdapter{runs: &fakeRunLedger{err: errors.New("bead store unavailable")}}
	_, err := a.Label(context.Background(), targetsItem("gt-run1", "internal/a/a.go"))
	if err == nil || !strings.Contains(err.Error(), "bead store unavailable") {
		t.Fatalf("Label error = %v, want the store failure", err)
	}
}

func TestTargetsLabelNeedsTheRun(t *testing.T) {
	t.Parallel()
	a := &targetsAdapter{}
	if _, err := a.Label(context.Background(), Item{ID: "internal/a/a.go"}); err == nil {
		t.Fatal("Label succeeded on an item with no run, want an error")
	}
}

func TestTargetsBatteryIsRegistered(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	a, err := Open(TargetsBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	if a.Battery() != TargetsBattery {
		t.Errorf("battery = %q, want %q", a.Battery(), TargetsBattery)
	}
	if got := a.LogPath(); got != targets.LogPath(town) {
		t.Errorf("log = %q, want the targets log %q", got, targets.LogPath(town))
	}
	if _, ok := a.(Contextual); !ok {
		t.Error("the targets adapter does not report context")
	}
}

// targetsLog is the fixture log: three runs, one of them still open, one line
// from another question set and one line that does not parse. The lines of a
// run are in the slice's risk order.
var targetsLog = []string{
	`{"ts":"2026-10-19T09:00:00Z","run":"gt-run1","path":"internal/a/a.go","tier":1,"risk_score":9.5,"bytes":1200,"model":"jev-1.13.0","answers":{"likely_defect":{"noul":0.9},"logic_churn":{"noul":0.7},"under_tested":{"noul":0.6},"review_priority":{"score":2,"confidence":0.7}},"question_version":1}`,
	`{"ts":"2026-10-19T09:00:01Z","run":"gt-run1","path":"internal/b/b.go","tier":1,"risk_score":8.1,"bytes":900,"model":"jev-1.13.0","answers":{"likely_defect":{"noul":0.4},"logic_churn":{"noul":0.3},"under_tested":{"noul":0.2},"review_priority":{"score":2,"confidence":0.6}},"question_version":1}`,
	`{"ts":"2026-10-19T09:00:02Z","run":"gt-run1","path":"internal/c/c.go","tier":1,"risk_score":7.4,"bytes":700,"model":"jev-1.13.0","answers":{"likely_defect":{"noul":0.7},"logic_churn":{"noul":0.8},"under_tested":{"noul":0.5},"review_priority":{"score":0,"confidence":0.6}},"question_version":1}`,
	`{"ts":"2026-10-19T09:00:03Z","run":"gt-run1","path":"internal/d/d.go","tier":0,"risk_score":6.2,"bytes":600,"model":"jev-1.13.0","answers":{"likely_defect":{"noul":0.6},"logic_churn":{"noul":0.5},"under_tested":{"noul":0.4},"review_priority":{"score":1,"confidence":0.5}},"question_version":1}`,
	`{"ts":"2026-10-19T09:10:00Z","run":"gt-run2","path":"internal/e/e.go","tier":0,"risk_score":5.5,"bytes":500,"model":"jev-1.13.0","answers":{"likely_defect":{"noul":0.3},"logic_churn":{"noul":0.2},"under_tested":{"noul":0.7},"review_priority":{"score":1,"confidence":0.4}},"question_version":1}`,
	`{"ts":"2026-10-19T09:10:01Z","run":"gt-run2","path":"internal/f/f.go","tier":0,"risk_score":4.0,"bytes":400,"model":"jev-1.13.0","answers":{"likely_defect":{"noul":0.8},"logic_churn":{"noul":0.6},"under_tested":{"noul":0.9},"review_priority":{"score":0,"confidence":0.5}},"question_version":1}`,
	`{"ts":"2026-10-19T09:20:00Z","run":"gt-run3","path":"internal/g/g.go","tier":0,"risk_score":3.2,"bytes":300,"model":"jev-1.13.0","answers":{"likely_defect":{"noul":0.5},"logic_churn":{"noul":0.4},"under_tested":{"noul":0.3},"review_priority":{"score":2,"confidence":0.5}},"question_version":1}`,
	`{"ts":"2026-10-19T09:20:01Z","run":"gt-run3","path":"internal/h/h.go","question_version":99}`,
	`not json`,
}

// targetsRunBeads is the fixture's run beads: one closed with a finding, one
// closed with none, one still open.
var targetsRunBeads = map[string]runBead{
	"gt-run1": {
		Found:  true,
		Status: "closed",
		Notes: strings.Join([]string{
			"cursor: 2026-10-19T08:00:00Z internal/d/d.go",
			"reviewed: 2026-10-19 internal/a/a.go",
			"finding: internal/c/c.go:Check:wrong-branch gt-f1",
			"queued: internal/a/a.go:Do:bad P4 internal/a/a.go:1 the severity is not one",
		}, "\n"),
	},
	"gt-run2": {Found: true, Status: "closed", Notes: "reviewed: 2026-10-19 internal/e/e.go"},
	"gt-run3": {Found: true, Status: "open", Notes: "finding: internal/g/g.go:Run:bad gt-f2"},
}

// TestTargetsScorecardPrintsPerQuestionBlocks drives the adapter with fake run
// beads: the registry factory builds a live beads store, so the adapter is
// constructed with the fake its labeling reads instead.
func TestTargetsScorecardPrintsPerQuestionBlocks(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logPath := targets.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(targetsLog, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &targetsAdapter{log: logPath, runs: &fakeRunLedger{byBead: targetsRunBeads}}

	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 7 || rep.LinesSkipped != 2 {
		t.Fatalf("items %d skipped %d, want 7 and 2", rep.Items, rep.LinesSkipped)
	}
	wantContext := []string{
		"run gt-run1: 4 files scored, 1 with a finding; the first finding ranks 4 by Jev's review_priority, 3 by the slice's risk order",
		"run gt-run2: 2 files scored; no finding in the ledger",
		"run gt-run3: 1 file scored; the run bead is still open, so none is labeled",
	}
	if len(rep.Context) != len(wantContext) {
		t.Fatalf("context = %q, want %q", rep.Context, wantContext)
	}
	for i := range wantContext {
		if rep.Context[i] != wantContext[i] {
			t.Errorf("context[%d] = %q, want %q", i, rep.Context[i], wantContext[i])
		}
	}

	var out strings.Builder
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	goldenPath := filepath.Join("testdata", "targets.golden")
	if *updateTargetsGolden {
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
		t.Errorf("output differs from %s (rewrite with -update-targets-golden):\n%s\nwant:\n%s", goldenPath, got, want)
	}
}

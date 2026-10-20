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

	"github.com/steveyegge/gastown/internal/beads"
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

// scriptedStore serves scripted beads, and ErrNotFound for any other id.
type scriptedStore struct {
	issues map[string]*beads.Issue
	err    error
}

func (s *scriptedStore) Show(id string) (*beads.Issue, error) {
	if s.err != nil {
		return nil, s.err
	}
	issue, ok := s.issues[id]
	if !ok {
		return nil, beads.ErrNotFound
	}
	return issue, nil
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

func TestTargetsReviewedFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		file string
		ok   bool
	}{
		{
			name: "a reviewed line names its file",
			line: "reviewed: 2026-10-19 internal/c/c.go",
			file: "internal/c/c.go",
			ok:   true,
		},
		{name: "a reviewed line with no path is malformed", line: "reviewed: 2026-10-19"},
		{name: "a reviewed line with a bad date is malformed", line: "reviewed: 2026-13-45 internal/c/c.go"},
		{name: "a reviewed line with no date is malformed", line: "reviewed: internal/c/c.go"},
		{name: "a cursor line is not a reviewed line", line: "cursor: 2026-10-19T08:00:00Z internal/c/c.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file, ok := reviewedFile(tc.line)
			if ok != tc.ok || file != tc.file {
				t.Errorf("reviewedFile(%q) = %q, %v, want %q, %v", tc.line, file, ok, tc.file, tc.ok)
			}
		})
	}
}

// scriptLedgerShapes returns each ledger line template the header block of
// scripts/deep-review.sh defines, keyed by line name.
func scriptLedgerShapes(src string) map[string]string {
	shapes := map[string]string{}
	in := false
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "# Ledger lines") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if strings.HasPrefix(line, "# --prev-notes") {
			break
		}
		if !strings.HasPrefix(line, "#") {
			break
		}
		body, ok := strings.CutPrefix(line, "#   ")
		if !ok || strings.HasPrefix(body, " ") {
			continue // a continuation line, indented past the shape
		}
		name, tmpl, ok := strings.Cut(body, ": ")
		if !ok {
			continue
		}
		shapes[name] = tmpl
	}
	return shapes
}

// TestTargetsParserMirrorsTheScriptLedger checks the hand-written parser
// accepts an instance of each ledger shape scripts/deep-review.sh's header
// defines: a shape the script changes fails here.
func TestTargetsParserMirrorsTheScriptLedger(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "deep-review.sh"))
	if err != nil {
		t.Fatal(err)
	}
	shapes := scriptLedgerShapes(string(src))
	values := map[string]string{
		"<YYYY-MM-DD>":            "2026-10-18",
		"<path>":                  "internal/a/a.go",
		"<fingerprint>":           "internal/a/a.go:Do:missing-guard",
		"<bead id | digest-only>": "gt-f1",
		"<P1|P2|P3>":              "P2",
		"<file:line>":             "internal/a/a.go:12",
		"<claim>":                 "the guard is missing",
	}
	cases := []struct {
		shape string
		parse func(string) (string, bool)
	}{
		{"reviewed", reviewedFile},
		{"finding", ledgerLineFile},
		{"queued", ledgerLineFile},
		{"p3", ledgerLineFile},
	}
	for _, tc := range cases {
		t.Run(tc.shape, func(t *testing.T) {
			t.Parallel()
			tmpl, ok := shapes[tc.shape]
			if !ok {
				t.Fatalf("scripts/deep-review.sh no longer defines a %s ledger line", tc.shape)
			}
			example := tc.shape + ": " + tmpl
			for placeholder, value := range values {
				example = strings.ReplaceAll(example, placeholder, value)
			}
			if strings.Contains(example, "<") {
				t.Fatalf("the %s shape %q holds a placeholder this test does not fill", tc.shape, tmpl)
			}
			file, ok := tc.parse(example)
			if !ok || file != "internal/a/a.go" {
				t.Errorf("parse %q = %q, %v, want internal/a/a.go", example, file, ok)
			}
		})
	}
}

func TestTargetsLabelReadsTheRunLedger(t *testing.T) {
	t.Parallel()
	notes := strings.Join([]string{
		"cursor: 2026-10-19T08:00:00Z internal/d/d.go",
		"reviewed: 2026-10-19 internal/a/a.go",
		"reviewed: 2026-10-19 internal/b/b.go",
		"reviewed: 2026-10-19 internal/c/c.go",
		"reviewed: 2026-10-19 internal/d/d.go",
		"reviewed: 2026-10-19 internal/e/e.go",
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
		{name: "a reviewed file with no finding line is clean", bead: closed, path: "internal/d/d.go", labeled: true},
		{name: "a file only a malformed line names is clean", bead: closed, path: "internal/a/a.go", labeled: true},
		{name: "a file the ledger does not name stays unlabeled", bead: closed, path: "internal/z/z.go"},
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
		{
			name: "a closed run with empty notes labels nothing",
			bead: runBead{Found: true, Status: "closed"},
			path: "internal/c/c.go",
		},
		{
			name: "a closed run whose notes hold no ledger line labels nothing",
			bead: runBead{Found: true, Status: "closed", Notes: "garbage\nfinding: nope"},
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

// targetTown writes a town whose gt- prefix routes to a gastown rig.
func targetTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := beads.WriteRoutes(filepath.Join(town, ".beads"), []beads.Route{{Prefix: "gt-", Path: "gastown"}}); err != nil {
		t.Fatal(err)
	}
	return town
}

func TestTargetsTownRunsReadsEachRigsStore(t *testing.T) {
	t.Parallel()
	town := targetTown(t)
	store := &scriptedStore{issues: map[string]*beads.Issue{
		"gt-run1": {ID: "gt-run1", Status: "closed", Notes: "reviewed: 2026-10-19 internal/a/a.go"},
		"hq-run1": {ID: "hq-run1", Status: "open"},
	}}
	var dirs []string
	runs := &townRuns{town: town, stores: map[string]beadShower{}, store: func(dir string) beadShower {
		dirs = append(dirs, dir)
		return store
	}}

	got, err := runs.Read("gt-run1")
	if err != nil {
		t.Fatal(err)
	}
	want := runBead{Found: true, Status: "closed", Notes: "reviewed: 2026-10-19 internal/a/a.go"}
	if got != want {
		t.Errorf("Read(gt-run1) = %+v, want %+v", got, want)
	}
	if wantDir := filepath.Join(town, "gastown", ".beads"); len(dirs) != 1 || dirs[0] != wantDir {
		t.Errorf("opened %q, want the gastown store %q", dirs, wantDir)
	}

	if _, err := runs.Read("hq-run1"); err != nil {
		t.Fatal(err)
	}
	if wantDir := filepath.Join(town, ".beads"); len(dirs) != 2 || dirs[1] != wantDir {
		t.Errorf("opened %q, want the town store %q second", dirs, wantDir)
	}

	// Each rig's store opens once, and every read of that rig reuses it.
	for _, bead := range []string{"gt-run1", "hq-run1"} {
		if _, err := runs.Read(bead); err != nil {
			t.Fatal(err)
		}
	}
	if len(dirs) != 2 {
		t.Errorf("opened %q, want one store per rig", dirs)
	}

	got, err = runs.Read("gt-gone")
	if err != nil || got != (runBead{}) {
		t.Errorf("Read(gt-gone) = %+v, %v, want an unlabeled missing bead", got, err)
	}
}

func TestTargetsTownRunsFailsOnAStoreError(t *testing.T) {
	t.Parallel()
	runs := &townRuns{town: targetTown(t), stores: map[string]beadShower{}, store: func(string) beadShower {
		return &scriptedStore{err: errors.New("bead store unavailable")}
	}}
	_, err := runs.Read("gt-run1")
	if err == nil || !strings.Contains(err.Error(), "bead store unavailable") {
		t.Fatalf("Read error = %v, want the store failure", err)
	}
}

// TestTargetsBatteryLabelsThroughTheTownStores drives the registered factory
// end to end: the adapter it builds reads each run bead through the store of
// the rig its id routes to.
func TestTargetsBatteryLabelsThroughTheTownStores(t *testing.T) {
	t.Parallel()
	town := targetTown(t)
	store := &scriptedStore{issues: map[string]*beads.Issue{
		"gt-run1": {ID: "gt-run1", Status: "closed", Notes: strings.Join([]string{
			"reviewed: 2026-10-19 internal/a/a.go",
			"finding: internal/b/b.go:Do:bad gt-f1",
		}, "\n")},
	}}
	a, err := Open(TargetsBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	runs := a.(*targetsAdapter).runs.(*townRuns)
	var dirs []string
	runs.store = func(dir string) beadShower {
		dirs = append(dirs, dir)
		return store
	}

	cases := []struct {
		path    string
		labeled bool
		found   bool
	}{
		{path: "internal/b/b.go", labeled: true, found: true},
		{path: "internal/a/a.go", labeled: true},
		{path: "internal/z/z.go"},
	}
	for _, tc := range cases {
		lab, err := a.Label(context.Background(), targetsItem("gt-run1", tc.path))
		if err != nil {
			t.Fatal(err)
		}
		if lab.Labeled != tc.labeled {
			t.Fatalf("%s labeled = %v, want %v", tc.path, lab.Labeled, tc.labeled)
		}
		if tc.labeled && lab.Labels[targets.QLikelyDefect] != tc.found {
			t.Errorf("%s likely_defect = %v, want %v", tc.path, lab.Labels[targets.QLikelyDefect], tc.found)
		}
	}
	wantDir := filepath.Join(town, "gastown", ".beads")
	if len(dirs) != 1 || dirs[0] != wantDir {
		t.Errorf("opened %q, want the gastown store %q once", dirs, wantDir)
	}
}

// targetsLog is the fixture log: five runs, one of them still open, one whose
// ledger names only some of its files, one whose ledger is empty, one line
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
	`{"ts":"2026-10-19T09:30:00Z","run":"gt-run4","path":"internal/h/h.go","tier":0,"risk_score":2.6,"bytes":260,"model":"jev-1.13.0","answers":{"likely_defect":{"noul":0.2},"logic_churn":{"noul":0.1},"under_tested":{"noul":0.6},"review_priority":{"score":1,"confidence":0.4}},"question_version":1}`,
	`{"ts":"2026-10-19T09:30:01Z","run":"gt-run4","path":"internal/i/i.go","tier":0,"risk_score":2.1,"bytes":210,"model":"jev-1.13.0","answers":{"likely_defect":{"noul":0.3},"logic_churn":{"noul":0.4},"under_tested":{"noul":0.2},"review_priority":{"score":0,"confidence":0.4}},"question_version":1}`,
	`{"ts":"2026-10-19T09:40:00Z","run":"gt-run5","path":"internal/j/j.go","tier":0,"risk_score":1.4,"bytes":140,"model":"jev-1.13.0","answers":{"likely_defect":{"noul":0.6},"logic_churn":{"noul":0.5},"under_tested":{"noul":0.3},"review_priority":{"score":2,"confidence":0.5}},"question_version":1}`,
	`not json`,
}

// targetsRunBeads is the fixture's run beads: closed runs with a finding, with
// none, with a ledger that names only some files, and with an empty ledger, and
// one run still open.
var targetsRunBeads = map[string]runBead{
	"gt-run1": {
		Found:  true,
		Status: "closed",
		Notes: strings.Join([]string{
			"cursor: 2026-10-19T08:00:00Z internal/d/d.go",
			"reviewed: 2026-10-19 internal/a/a.go",
			"reviewed: 2026-10-19 internal/b/b.go",
			"reviewed: 2026-10-19 internal/c/c.go",
			"reviewed: 2026-10-19 internal/d/d.go",
			"finding: internal/c/c.go:Check:wrong-branch gt-f1",
			"queued: internal/a/a.go:Do:bad P4 internal/a/a.go:1 the severity is not one",
		}, "\n"),
	},
	"gt-run2": {
		Found:  true,
		Status: "closed",
		Notes:  "reviewed: 2026-10-19 internal/e/e.go\nreviewed: 2026-10-19 internal/f/f.go",
	},
	"gt-run3": {Found: true, Status: "open", Notes: "finding: internal/g/g.go:Run:bad gt-f2"},
	"gt-run4": {Found: true, Status: "closed", Notes: "reviewed: 2026-10-19 internal/h/h.go"},
	"gt-run5": {Found: true, Status: "closed"},
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
	if rep.Items != 10 || rep.LinesSkipped != 2 {
		t.Fatalf("items %d skipped %d, want 10 and 2", rep.Items, rep.LinesSkipped)
	}
	wantContext := []string{
		"run gt-run1: 4 files scored, 1 with a finding; the first finding ranks 4 by Jev's review_priority, 3 by the slice's risk order",
		"run gt-run2: 2 files scored; no finding in the ledger",
		"run gt-run3: 1 file scored; the run bead is still open, so none is labeled",
		"run gt-run4: 2 files scored, 1 the ledger does not name; no finding in the ledger",
		"run gt-run5: 1 file scored; the ledger names none of them, so none is labeled",
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

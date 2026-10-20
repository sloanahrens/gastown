package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
)

// findingBeads serves beads from a map. A missing id is beads.ErrNotFound.
type findingBeads struct {
	issues map[string]*beads.Issue
	err    error
}

func (f findingBeads) Show(id string) (*beads.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	if b, ok := f.issues[id]; ok {
		return b, nil
	}
	return nil, beads.ErrNotFound
}

// findingDiffs sizes commits from a map. A commit not in it is ErrNoCommit.
type findingDiffs struct {
	lines map[string]int
	err   error
}

func (f findingDiffs) ChangedLines(_ context.Context, commit string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	if n, ok := f.lines[commit]; ok {
		return n, nil
	}
	return 0, ErrNoCommit
}

const landedSHA = "0123abc4567def"

// landedReason is the close reason the landing worker writes.
func landedReason(sha string) string {
	return fmt.Sprintf("Landed on main\ntarget_branch: main\ncommit_sha: %s\nlanded_commit: %s\npatch_id: p1", sha, sha)
}

func closedBead(id, reason string) *beads.Issue {
	return &beads.Issue{ID: id, Status: "closed", CloseReason: reason}
}

func fptr(v float64) *float64 { return &v }
func sptr(v string) *string   { return &v }

// findingItem is a parsed log line for a finding that offered candidates and
// whose duplicate Choice picked chose.
func findingItem(t *testing.T, a *finding, id, chose string, offered ...string) Item {
	t.Helper()
	line := dedupe.Line{
		TS: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC), Finding: id, CandidatesOffered: offered,
		Outcome: dedupe.OutcomeAnswered, Model: "jev-1",
		Answers: map[string]dedupe.Answer{
			dedupe.QFixWorthy: {Noul: fptr(0.8)},
			dedupe.QSeverity:  {Score: fptr(2), Confidence: fptr(0.7)},
		},
	}
	if chose != "" {
		line.Answers[dedupe.QDuplicate] = dedupe.Answer{Choice: sptr(chose), Confidence: fptr(0.9)}
	}
	return parseOne(t, a, line)
}

func parseOne(t *testing.T, a *finding, line dedupe.Line) Item {
	t.Helper()
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	item, err := a.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return item
}

func newFinding(b BeadReader, d DiffSizer) *finding {
	return NewFinding(t0Town, b, d).(*finding)
}

const t0Town = "/town"

func TestFindingCloseReasonLabels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		bead    *beads.Issue
		offered []string
		chose   string
		want    map[QuestionID]bool
	}{
		{
			name: "landed is fix worthy and not report only",
			bead: closedBead("gt-f1", "Landed on main\ntarget_branch: main"),
			want: map[QuestionID]bool{"fix_worthy": true, "report_only": false, "any_duplicate": false},
		},
		{
			name: "no-changes is report only and not fix worthy",
			bead: closedBead("gt-f1", "no-changes: can't reproduce"),
			want: map[QuestionID]bool{"fix_worthy": false, "report_only": true, "any_duplicate": false},
		},
		{
			name: "superseded wording is not fix worthy",
			bead: closedBead("gt-f1", "superseded by the rewrite"),
			want: map[QuestionID]bool{"fix_worthy": false},
		},
		{
			name: "duplicate wording naming nothing offered leaves any_duplicate unlabeled",
			bead: closedBead("gt-f1", "Duplicate of gt-zzz"),
			want: map[QuestionID]bool{"fix_worthy": false},
		},
		{
			name: "wontfix wording is not fix worthy",
			bead: closedBead("gt-f1", "WONTFIX: by design"),
			want: map[QuestionID]bool{"fix_worthy": false},
		},
		{
			name: "won't fix wording is not fix worthy",
			bead: closedBead("gt-f1", "won't fix, intended"),
			want: map[QuestionID]bool{"fix_worthy": false},
		},
		{
			name: "no-changes wording a duplicate stays not fix worthy and report only",
			bead: closedBead("gt-f1", "no-changes: duplicate"),
			want: map[QuestionID]bool{"fix_worthy": false, "report_only": true},
		},
		{
			name:    "a close reason naming an offered candidate is any_duplicate, and the Choice naming it is correct",
			bead:    closedBead("gt-f1", "duplicate of gt-c2"),
			offered: []string{"gt-c1", "gt-c2"},
			chose:   "gt-c2",
			want:    map[QuestionID]bool{"fix_worthy": false, "any_duplicate": true, "duplicate": true},
		},
		{
			name:    "the Choice picking another candidate is wrong",
			bead:    closedBead("gt-f1", "duplicate of gt-c2"),
			offered: []string{"gt-c1", "gt-c2"},
			chose:   "gt-c1",
			want:    map[QuestionID]bool{"fix_worthy": false, "any_duplicate": true, "duplicate": false},
		},
		{
			name:    "the Choice picking none when a candidate is named is wrong",
			bead:    closedBead("gt-f1", "superseded by gt-c1"),
			offered: []string{"gt-c1"},
			chose:   "none",
			want:    map[QuestionID]bool{"fix_worthy": false, "any_duplicate": true, "duplicate": false},
		},
		{
			name:    "a landed bead the Choice called none is correct",
			bead:    closedBead("gt-f1", landedReason(landedSHA)),
			offered: []string{"gt-c1"},
			chose:   "none",
			want:    map[QuestionID]bool{"fix_worthy": true, "report_only": false, "any_duplicate": false, "duplicate": true},
		},
		{
			name:    "a landed bead the Choice matched to a candidate is wrong",
			bead:    closedBead("gt-f1", landedReason(landedSHA)),
			offered: []string{"gt-c1"},
			chose:   "gt-c1",
			want:    map[QuestionID]bool{"fix_worthy": true, "report_only": false, "any_duplicate": false, "duplicate": false},
		},
		{
			name:    "a candidate id inside a longer id is not named",
			bead:    closedBead("gt-f1", "no-changes: see gt-c10 and gt-c1.2"),
			offered: []string{"gt-c1"},
			want:    map[QuestionID]bool{"fix_worthy": false, "report_only": true, "any_duplicate": false},
		},
		{
			name:    "a candidate named with different case and trailing punctuation is named",
			bead:    closedBead("gt-f1", "Superseded by GT-C1."),
			offered: []string{"gt-c1"},
			want:    map[QuestionID]bool{"fix_worthy": false, "any_duplicate": true},
		},
		{
			name: "a reason of no known shape labels nothing",
			bead: closedBead("gt-f1", "done"),
			want: map[QuestionID]bool{},
		},
		{
			name: "an open bead is unlabeled",
			bead: &beads.Issue{ID: "gt-f1", Status: "open"},
			want: nil,
		},
		{
			name: "an in-progress bead with an old close reason is unlabeled",
			bead: &beads.Issue{ID: "gt-f1", Status: "in_progress", CloseReason: "Landed on main"},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newFinding(findingBeads{issues: map[string]*beads.Issue{"gt-f1": tc.bead}}, findingDiffs{})
			item := findingItem(t, a, "gt-f1", tc.chose, tc.offered...)
			lab, err := a.Label(context.Background(), item)
			if err != nil {
				t.Fatalf("Label: %v", err)
			}
			got, labeled := lab.Labels, lab.Labeled
			if labeled != (len(tc.want) > 0) {
				t.Errorf("labeled = %v, want %v (labels %v)", labeled, len(tc.want) > 0, got)
			}
			// A landed reason also names a commit this fake cannot size.
			delete(got, dedupe.QSeverity)
			if !maps.Equal(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Errorf("labels = %v, want %v", got, tc.want)
			}
		})
	}
}

// --json names the rule that labeled a line: the close reason's class.
func TestFindingLabelRuleNames(t *testing.T) {
	t.Parallel()
	cases := []struct{ reason, want string }{
		{"Landed on main", "close:landed"},
		{"no-changes: nothing to do", "close:no-changes"},
		{"superseded by gt-zzz", "close:superseded-duplicate-wontfix"},
		{"see gt-c1", "close:names-candidate"},
		{"landed, see gt-c1", "close:landed"},
	}
	for _, tc := range cases {
		a := newFinding(findingBeads{issues: map[string]*beads.Issue{"gt-f1": closedBead("gt-f1", tc.reason)}}, findingDiffs{})
		lab, err := a.Label(context.Background(), findingItem(t, a, "gt-f1", "", "gt-c1"))
		if err != nil || lab.Rule != tc.want {
			t.Errorf("close reason %q gave rule %q (error %v), want %q", tc.reason, lab.Rule, err, tc.want)
		}
	}
}

func TestFindingSeverityLabelFromLandedDiffSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		lines   int
		reason  string
		want    bool
		labeled bool
	}{
		{"49 lines is small", 49, landedReason(landedSHA), false, true},
		{"50 lines is neither", 50, landedReason(landedSHA), false, false},
		{"51 lines is neither", 51, landedReason(landedSHA), false, false},
		{"300 lines is neither", 300, landedReason(landedSHA), false, false},
		{"301 lines is large", 301, landedReason(landedSHA), true, true},
		{"an empty diff is small", 0, landedReason(landedSHA), false, true},
		{"a landed reason naming no commit leaves it unlabeled", 400, "Landed on main", false, false},
		{"a commit the repo lacks leaves it unlabeled", 400, landedReason("deadbee"), false, false},
		{"a bare commit_sha line is read too", 400, "Landed on main\ncommit_sha: " + landedSHA, true, true},
		{"a no-changes close names no landing", 400, "no-changes: commit_sha: " + landedSHA, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newFinding(
				findingBeads{issues: map[string]*beads.Issue{"gt-f1": closedBead("gt-f1", tc.reason)}},
				findingDiffs{lines: map[string]int{landedSHA: tc.lines}},
			)
			lab, err := a.Label(context.Background(), findingItem(t, a, "gt-f1", ""))
			if err != nil {
				t.Fatalf("Label: %v", err)
			}
			v, ok := lab.Labels[dedupe.QSeverity]
			if ok != tc.labeled || v != tc.want {
				t.Errorf("severity label = (%v, %v) for %d lines, want (%v, %v)", v, ok, tc.lines, tc.want, tc.labeled)
			}
		})
	}
}

// A bead that is gone leaves the line unlabeled, but a source that cannot be
// read fails the report.
func TestFindingLabelSourceErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	gone := newFinding(findingBeads{}, findingDiffs{})
	if lab, err := gone.Label(ctx, findingItem(t, gone, "gt-f1", "")); err != nil || lab.Labeled {
		t.Errorf("a bead that does not exist gave labeled %v, error %v; want unlabeled and no error", lab.Labeled, err)
	}

	broken := newFinding(findingBeads{err: errors.New("dolt down")}, findingDiffs{})
	if _, err := broken.Label(ctx, findingItem(t, broken, "gt-f1", "")); err == nil || !strings.Contains(err.Error(), "dolt down") {
		t.Errorf("an unreadable bead store gave error %v, want the cause", err)
	}

	noGit := newFinding(
		findingBeads{issues: map[string]*beads.Issue{"gt-f1": closedBead("gt-f1", landedReason(landedSHA))}},
		findingDiffs{err: errors.New("repo unreadable")},
	)
	if _, err := noGit.Label(ctx, findingItem(t, noGit, "gt-f1", "")); err == nil || !strings.Contains(err.Error(), "repo unreadable") {
		t.Errorf("an unreadable repo gave error %v, want the cause", err)
	}
}

func TestFindingParse(t *testing.T) {
	t.Parallel()
	a := newFinding(findingBeads{}, findingDiffs{})
	raw := `{"ts":"2026-10-18T09:00:00Z","finding":"gt-f1","candidate":"gt-c1","confidence":0.9,` +
		`"candidates_offered":["gt-c1"],"outcome":"answered","model":"jev-1","question_version":1,` +
		`"answers":{"duplicate":{"choice":"gt-c1","confidence":0.9},"any_duplicate":{"noul":0.8},` +
		`"severity":{"score":2,"confidence":0.6}}}`
	item, err := a.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "gt-f1" || item.Model != "jev-1" || !item.Time.Equal(time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("item = %+v", item)
	}
	dup := item.Answers["duplicate"]
	if dup.Choice != "gt-c1" || dup.Positive != "gt-c1" || dup.Probabilities["gt-c1"] != 0.9 {
		t.Errorf("duplicate answer = %+v, want the chosen option as positive with its confidence", dup)
	}
	if n := item.Answers["any_duplicate"].Noul; n == nil || *n != 0.8 {
		t.Errorf("any_duplicate answer = %+v", item.Answers["any_duplicate"])
	}
	if s := item.Answers["severity"].Score; s == nil || *s != 2 {
		t.Errorf("severity answer = %+v", item.Answers["severity"])
	}

	// A line whose call failed has null answers and still parses.
	failed, err := a.Parse([]byte(`{"ts":"2026-10-18T09:00:00Z","finding":"gt-f2","candidate":"none","outcome":"ask_failed","answers":null}`))
	if err != nil || len(failed.Answers) != 0 {
		t.Errorf("failed-call line gave %+v, %v; want an item with no answers", failed, err)
	}

	for _, bad := range []string{`not json`, `{"ts":"2026-10-18T09:00:00Z"}`, `{"finding":"gt-f1"}`} {
		if _, err := a.Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", bad)
		}
	}
}

func TestFindingMentions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text, id string
		want     bool
	}{
		{"duplicate of gt-ab", "gt-ab", true},
		{"gt-ab", "gt-ab", true},
		{"(gt-ab)", "gt-ab", true},
		{"see gt-ab, then", "gt-ab", true},
		{"see gt-ab.", "gt-ab", true},
		{"see gt-abc", "gt-ab", false},
		{"see gt-ab.1", "gt-ab", false},
		{"see xgt-ab", "gt-ab", false},
		{"see gt-ab-2", "gt-ab", false},
		{"see gt-abc and gt-ab", "gt-ab", true},
		{"nothing", "gt-ab", false},
	}
	for _, tc := range cases {
		if got := mentions(tc.text, tc.id); got != tc.want {
			t.Errorf("mentions(%q, %q) = %v, want %v", tc.text, tc.id, got, tc.want)
		}
	}
}

func TestFindingNumstatLines(t *testing.T) {
	t.Parallel()
	out := "10\t2\tinternal/a.go\n-\t-\tlogo.png\n3\t0\tdocs/x.md\n\nmalformed\n"
	if got := numstatLines(out); got != 15 {
		t.Errorf("numstatLines = %d, want 15 (binary counts 0)", got)
	}
}

// The registered adapter reads the dedupe log under the town.
func TestFindingRegisteredLogPath(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	a, err := Open(FindingBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	if a.Battery() != "finding" || a.LogPath() != dedupe.LogPath(town) {
		t.Errorf("adapter = %q at %q, want finding at %q", a.Battery(), a.LogPath(), dedupe.LogPath(town))
	}
}

// Build scores each question of a finding log against the beads' close
// reasons: an open bead and a missing one are logged but not labeled.
func TestFindingBuildScoresQuestions(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	log := []string{
		findingLogLine("2026-10-18T09:00:00Z", "gt-a", `"gt-c1"`, 0.9, 0.8),
		findingLogLine("2026-10-18T10:00:00Z", "gt-b", `"gt-c1"`, 0.8, 0.3),
		findingLogLine("2026-10-18T11:00:00Z", "gt-open", `"none"`, 0.7, 0.4),
		findingLogLine("2026-10-18T12:00:00Z", "gt-gone", `"none"`, 0.7, 0.4),
		`not json`,
	}
	path := dedupe.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(log, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewFinding(town, findingBeads{issues: map[string]*beads.Issue{
		"gt-a":    closedBead("gt-a", "duplicate of gt-c1"),
		"gt-b":    closedBead("gt-b", landedReason(landedSHA)),
		"gt-open": {ID: "gt-open", Status: "open"},
	}}, findingDiffs{lines: map[string]int{landedSHA: 400}})
	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 4 || rep.LinesSkipped != 1 {
		t.Fatalf("items %d skipped %d, want 4 and 1", rep.Items, rep.LinesSkipped)
	}

	fix := question(t, rep, "fix_worthy")
	if fix.Logged != 4 || fix.Labeled != 2 || fix.Positives != 1 || fix.Negatives != 1 {
		t.Errorf("fix_worthy logged %d labeled %d positives %d negatives %d, want 4, 2, 1, 1", fix.Logged, fix.Labeled, fix.Positives, fix.Negatives)
	}
	dup := question(t, rep, "duplicate")
	if dup.Kind != KindChoice || dup.Labeled != 2 || dup.Positives != 1 {
		t.Errorf("duplicate kind %s labeled %d positives %d, want choice, 2, 1", dup.Kind, dup.Labeled, dup.Positives)
	}
	sev := question(t, rep, "severity")
	if sev.Kind != KindScore || sev.Labeled != 1 || sev.Positives != 1 {
		t.Errorf("severity kind %s labeled %d positives %d, want score, 1, 1 (only gt-b landed)", sev.Kind, sev.Labeled, sev.Positives)
	}
}

// findingLogLine is a dedupe log line offering gt-c1, with a duplicate Choice,
// a fix_worthy noul and a severity score.
func findingLogLine(ts, finding, choice string, confidence, fixWorthy float64) string {
	return fmt.Sprintf(`{"ts":%q,"finding":%q,"candidate":%s,"confidence":%v,"candidates_offered":["gt-c1"],`+
		`"outcome":"answered","model":"jev-1","question_version":1,"answers":{`+
		`"duplicate":{"choice":%s,"confidence":%v},"fix_worthy":{"noul":%v},"severity":{"score":2,"confidence":0.5}}}`,
		ts, finding, choice, confidence, choice, confidence, fixWorthy)
}

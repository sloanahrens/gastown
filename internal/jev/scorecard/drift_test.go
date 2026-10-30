package scorecard

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/drift"
)

// driftDoc is a doc with a section after its first heading, whose one body line
// is line 5.
const driftDoc = "# Doc\n\n## Drift battery\n\nText.\n"

// driftAsk is the instant the drift answers in these tests were logged.
var driftAsk = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// fakeDocs serves each doc's edits from memory, or fails with err.
type fakeDocs struct {
	edits map[string][]docEdit
	err   error
}

func (f fakeDocs) Edits(_ context.Context, doc string) ([]docEdit, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.edits[doc], nil
}

// fakeDriftBeads serves each rig's beads from memory, or fails with err. It
// returns every bead, so the window is the adapter's to apply, as it is for the
// real store's listing.
type fakeDriftBeads struct {
	byRig map[string][]*beads.Issue
	err   error
}

func (f fakeDriftBeads) List(rig string, _ beads.ListOptions) ([]*beads.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byRig[rig], nil
}

var updateDriftGolden = flag.Bool("update-drift-golden", false, "rewrite testdata/drift.golden")

// driftReferenceV1 is the doc the fixture history starts from: a drift section
// and a scorecard section.
const driftReferenceV1 = `# Reference

## Drift battery

The battery asks Jev about each section.

## Scorecard

The scorecard reads each log.
`

// driftReferenceV2 adds a flag to the drift section, which is the edit the
// fixture's drift answers are checked against.
const driftReferenceV2 = `# Reference

## Drift battery

The battery asks Jev about each section.
Pass ` + "`--recent`" + ` to widen the window.

## Scorecard

The scorecard reads each log.
`

// driftFixtureEdits is the fixture history the golden report reads, as the docs
// reader returns it: the commit that adds the doc, then the one that adds the
// flag to the drift section. The second edit's hunk is the insertion after line
// 5 of V1, the line git's zero-context diff would give.
var driftFixtureEdits = map[string][]docEdit{
	"docs/reference.md": {
		{
			Time:  time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			Patch: "@@ -0,0 +1,9 @@\n+# Reference\n+\n+## Drift battery\n+\n+The battery asks Jev about each section.\n+\n+## Scorecard\n+\n+The scorecard reads each log.\n",
		},
		{
			Time:   time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
			Before: driftReferenceV1,
			Patch:  "@@ -5,0 +6 @@\n+Pass `--recent` to widen the window.\n",
		},
	},
}

// driftFixtureLog is the drift log the golden report reads. Its ask times and
// the fixture history put each section in one of the labeled states.
var driftFixtureLog = []string{
	// The drift section's edit touches it, and its flag is the one the edit names.
	`{"ts":"2026-09-01T00:00:00Z","bead":"gt-a","rig":"gastown","question_version":1,"model":"m1","sections":[{"doc":"docs/reference.md","heading":"Drift battery","answers":{"doc_now_stale":{"noul":0.9},"drift_kind":{"choice":"flag","probabilities":{"flag":0.8,"command":0.1,"config_key":0.05,"behavior":0.05}}}}]}`,
	// Nothing edits the scorecard section, and its window has passed.
	`{"ts":"2026-09-01T00:00:00Z","bead":"gt-b","rig":"gastown","question_version":1,"model":"m1","sections":[{"doc":"docs/reference.md","heading":"Scorecard","answers":{"doc_now_stale":{"noul":0.2}}}]}`,
	// Asked inside its window; the bead landed in the window names the doc, so
	// the section counts as stale even though no commit touched it.
	`{"ts":"2026-10-15T00:00:00Z","bead":"gt-c","rig":"gastown","question_version":1,"model":"m1","sections":[{"doc":"docs/reference.md","heading":"Drift battery","answers":{"doc_now_stale":{"noul":0.6},"drift_kind":{"choice":"command","probabilities":{"command":0.7,"flag":0.2,"behavior":0.1}}}}]}`,
	// The same edit as gt-a, picked as a command, which the edit does not name.
	`{"ts":"2026-09-01T00:00:00Z","bead":"gt-d","rig":"gastown","question_version":1,"model":"m1","sections":[{"doc":"docs/reference.md","heading":"Drift battery","answers":{"doc_now_stale":{"noul":0.7},"drift_kind":{"choice":"command","probabilities":{"command":0.7,"flag":0.2,"behavior":0.1}}}}]}`,
	// A line from another question set is skipped whole.
	`{"ts":"2026-09-01T00:00:00Z","bead":"gt-e","rig":"gastown","question_version":0,"sections":[]}`,
	// A landing that names no docs-relevant section has no item.
	`{"ts":"2026-09-02T00:00:00Z","bead":"gt-f","rig":"gastown","question_version":1,"model":"m1","sections":[]}`,
}

// TestDriftScorecardPrintsPerQuestionBlocks is the drift battery's golden test.
// It reads the fixture log and the fixture history through the adapter, so the
// default tier exercises the report path. The git reader has its own test in
// the integration tier.
func TestDriftScorecardPrintsPerQuestionBlocks(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logPath := drift.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(driftFixtureLog, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// gt-c's bead landed inside its window and names the doc, so the bead rule
	// labels it stale even though no commit touched its section.
	landed := &beads.Issue{
		ID:          "gt-z",
		Title:       "docs: refresh docs/reference.md",
		Status:      statusClosed,
		CloseReason: "Landed on main (abc123)",
		ClosedAt:    time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
	a := newDriftAdapter(logPath,
		fakeDriftBeads{byRig: map[string][]*beads.Issue{"gastown": {landed}}},
		fakeDocs{edits: driftFixtureEdits},
		func() time.Time { return time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC) },
	)

	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 4 || rep.LinesSkipped != 1 {
		t.Fatalf("items %d skipped %d, want 4 and 1", rep.Items, rep.LinesSkipped)
	}

	var out strings.Builder
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	goldenPath := filepath.Join("testdata", "drift.golden")
	if *updateDriftGolden {
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
		t.Errorf("output differs from %s (rewrite with -update-drift-golden):\n%s\nwant:\n%s", goldenPath, got, want)
	}
}

// TestDriftLabelFailsWhenTheLabelCannotRead checks that a label whose history,
// bead listing or close time cannot be read is an error. A read that failed
// must not label the section as if it had looked.
func TestDriftLabelFailsWhenTheLabelCannotRead(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	now := driftAsk.Add(40 * day)
	badClose := &beads.Issue{
		ID:          "gt-bad",
		Title:       "docs: refresh docs/reference.md",
		Status:      statusClosed,
		CloseReason: "Landed on main (abc123)",
		ClosedAt:    "yesterday",
	}
	cases := []struct {
		name    string
		docs    fakeDocs
		beads   fakeDriftBeads
		wantErr string
	}{
		{
			name:    "the doc's history cannot be read",
			docs:    fakeDocs{err: errors.New("git log failed")},
			wantErr: "reading the history of docs/reference.md",
		},
		{
			name:    "the rig's beads cannot be listed",
			beads:   fakeDriftBeads{err: errors.New("bd list failed")},
			wantErr: "listing closed beads in gastown",
		},
		{
			name:    "a closed bead's close time does not parse",
			beads:   fakeDriftBeads{byRig: map[string][]*beads.Issue{"gastown": {badClose}}},
			wantErr: "close time \"yesterday\" that does not parse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newDriftAdapter("", &tc.beads, tc.docs, func() time.Time { return now })
			_, err := a.Label(context.Background(), driftItem(driftAsk, "Drift battery", ""))
			if err == nil {
				t.Fatal("Label returned no error, want one")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestDriftLabelBeadNamesWholeDocPath checks that the bead rule names the doc
// only as a whole path: a longer path that ends in the doc's path does not name
// it, and a full stop after the path does.
func TestDriftLabelBeadNamesWholeDocPath(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	closed := func(title string) *beads.Issue {
		return &beads.Issue{
			ID:          "gt-x",
			Title:       title,
			Status:      statusClosed,
			CloseReason: "Landed on main (abc123)",
			ClosedAt:    driftAsk.Add(5 * day).Format(time.RFC3339Nano),
		}
	}
	cases := []struct {
		name  string
		title string
		stale bool
		rule  string
	}{
		{name: "a path ending the title names the doc", title: "docs: refresh docs/reference.md", stale: true, rule: ruleDriftBeadLanded},
		{name: "a full stop after the path names the doc", title: "docs: refresh docs/reference.md.", stale: true, rule: ruleDriftBeadLanded},
		{name: "a longer directory that ends in the doc's path does not", title: "docs: fix old/docs/reference.md", stale: false, rule: ruleDriftNoEdit},
		{name: "a longer file name that starts with the doc's path does not", title: "docs: fix docs/reference.md.bak", stale: false, rule: ruleDriftNoEdit},
		{name: "a longer extension does not", title: "docs: fix docs/reference.mdx", stale: false, rule: ruleDriftNoEdit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := fakeDriftBeads{byRig: map[string][]*beads.Issue{"gastown": {closed(tc.title)}}}
			a := newDriftAdapter("", b, fakeDocs{}, func() time.Time { return driftAsk.Add(40 * day) })
			lab, err := a.Label(context.Background(), driftItem(driftAsk, "Drift battery", ""))
			if err != nil {
				t.Fatal(err)
			}
			if got := lab.Labels[qDocNowStale]; got != tc.stale {
				t.Errorf("doc_now_stale = %v, want %v", got, tc.stale)
			}
			if lab.Rule != tc.rule {
				t.Errorf("rule = %q, want %q", lab.Rule, tc.rule)
			}
		})
	}
}

// driftItem is the item a drift line makes for one section of
// docs/reference.md. An empty kind leaves drift_kind unanswered.
func driftItem(ask time.Time, heading, kind string) Item {
	return driftItemFor(ask, "docs/reference.md", heading, kind)
}

// driftItemFor is driftItem for one doc, for a test that asks about more than
// one.
func driftItemFor(ask time.Time, doc, heading, kind string) Item {
	answers := map[QuestionID]Answer{}
	if kind != "" {
		answers[qDriftKind] = Answer{Choice: kind, Positive: kind, Probabilities: map[string]float64{kind: 0.8}}
	}
	return Item{
		ID:      "gastown/gt-a#0",
		Time:    ask,
		Answers: answers,
		Refs: map[string]string{
			refRig:     "gastown",
			refBead:    "gt-a",
			refDoc:     doc,
			refSection: heading,
		},
	}
}

// driftEditAt is an edit that changes the body line of driftDoc's section.
func driftEditAt(at time.Time, patch string) docEdit {
	return docEdit{Time: at, Before: driftDoc, Patch: patch}
}

const driftBodyPatch = "@@ -5 +5 @@\n-Text.\n+Text now.\n"

func TestDriftLabelWindowEdges(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	cases := []struct {
		name    string
		edits   []docEdit
		now     time.Time
		labeled bool
		stale   bool
		rule    string
	}{
		{
			name:    "an edit at the ask's own instant is not after the ask",
			edits:   []docEdit{driftEditAt(driftAsk, driftBodyPatch)},
			now:     driftAsk.Add(31 * day),
			labeled: true,
			stale:   false,
			rule:    ruleDriftNoEdit,
		},
		{
			name:    "an edit one second after the ask makes the section stale",
			edits:   []docEdit{driftEditAt(driftAsk.Add(time.Second), driftBodyPatch)},
			now:     driftAsk.Add(31 * day),
			labeled: true,
			stale:   true,
			rule:    ruleDriftDocEdit,
		},
		{
			name:    "an edit at the window's last instant still counts",
			edits:   []docEdit{driftEditAt(driftAsk.Add(30*day), driftBodyPatch)},
			now:     driftAsk.Add(31 * day),
			labeled: true,
			stale:   true,
			rule:    ruleDriftDocEdit,
		},
		{
			name:    "an edit one second past the window does not count",
			edits:   []docEdit{driftEditAt(driftAsk.Add(30*day+time.Second), driftBodyPatch)},
			now:     driftAsk.Add(31 * day),
			labeled: true,
			stale:   false,
			rule:    ruleDriftNoEdit,
		},
		{
			name:    "no edit inside the window stays unlabeled",
			now:     driftAsk.Add(30*day - time.Second),
			labeled: false,
		},
		{
			name:    "no edit when the window has just passed is not stale",
			now:     driftAsk.Add(30 * day),
			labeled: true,
			stale:   false,
			rule:    ruleDriftNoEdit,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newDriftAdapter("", &fakeDriftBeads{}, fakeDocs{edits: map[string][]docEdit{"docs/reference.md": tc.edits}}, func() time.Time { return tc.now })
			lab, err := a.Label(context.Background(), driftItem(driftAsk, "Drift battery", ""))
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled != tc.labeled {
				t.Fatalf("labeled = %v, want %v (rule %q)", lab.Labeled, tc.labeled, lab.Rule)
			}
			if !tc.labeled {
				return
			}
			if got := lab.Labels[qDocNowStale]; got != tc.stale {
				t.Errorf("doc_now_stale = %v, want %v", got, tc.stale)
			}
			if lab.Rule != tc.rule {
				t.Errorf("rule = %q, want %q", lab.Rule, tc.rule)
			}
		})
	}
}

func TestDriftLabelBeadLandedNamingTheDoc(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	closed := func(at time.Time, reason, title string) *beads.Issue {
		return &beads.Issue{
			ID:          "gt-x",
			Title:       title,
			Status:      statusClosed,
			CloseReason: reason,
			ClosedAt:    at.Format(time.RFC3339Nano),
		}
	}
	const named = "docs: refresh the drift battery section in docs/reference.md"
	cases := []struct {
		name    string
		issue   *beads.Issue
		now     time.Time
		stale   bool
		rule    string
		labeled bool
	}{
		{
			name:    "a landed bead closed in the window that names the doc",
			issue:   closed(driftAsk.Add(5*day), "Landed on main (abc123)", named),
			now:     driftAsk.Add(40 * day),
			labeled: true, stale: true, rule: ruleDriftBeadLanded,
		},
		{
			name:    "a landed bead closed inside the window while it is still open",
			issue:   closed(driftAsk.Add(5*day), "landed", named),
			now:     driftAsk.Add(6 * day),
			labeled: true, stale: true, rule: ruleDriftBeadLanded,
		},
		{
			name:    "a bead closed after the window is not an edit",
			issue:   closed(driftAsk.Add(31*day), "Landed on main (abc123)", named),
			now:     driftAsk.Add(40 * day),
			labeled: true, stale: false, rule: ruleDriftNoEdit,
		},
		{
			name:    "a bead closed without landing is not an edit",
			issue:   closed(driftAsk.Add(5*day), "wontfix", named),
			now:     driftAsk.Add(40 * day),
			labeled: true, stale: false, rule: ruleDriftNoEdit,
		},
		{
			name:    "a landed bead that names another doc is not an edit",
			issue:   closed(driftAsk.Add(5*day), "Landed on main (abc123)", "docs: fix docs/scorecard.md"),
			now:     driftAsk.Add(40 * day),
			labeled: true, stale: false, rule: ruleDriftNoEdit,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := fakeDriftBeads{byRig: map[string][]*beads.Issue{"gastown": {tc.issue}}}
			a := newDriftAdapter("", b, fakeDocs{}, func() time.Time { return tc.now })
			lab, err := a.Label(context.Background(), driftItem(driftAsk, "Drift battery", ""))
			if err != nil {
				t.Fatal(err)
			}
			if !lab.Labeled {
				t.Fatalf("unlabeled, want a label (rule %q)", lab.Rule)
			}
			if got := lab.Labels[qDocNowStale]; got != tc.stale {
				t.Errorf("doc_now_stale = %v, want %v", got, tc.stale)
			}
			if lab.Rule != tc.rule {
				t.Errorf("rule = %q, want %q", lab.Rule, tc.rule)
			}
		})
	}
}

// TestNamesDoc checks the answers the whole-path rule has always given, so the
// faster path keeps them: the exact path names the doc, a longer path does not,
// a full stop or comma after it does, and a doc's regexp metacharacters are
// matched literally.
func TestNamesDoc(t *testing.T) {
	t.Parallel()
	names := newDocNameCache()
	cases := []struct {
		name string
		text string
		doc  string
		want bool
	}{
		{"the exact path", "docs: refresh the drift section in docs/reference.md", "docs/reference.md", true},
		{"the path alone", "docs/reference.md", "docs/reference.md", true},
		{"a longer path with a prefix directory", "docs: move old/docs/reference.md", "docs/reference.md", false},
		{"a longer path with a .bak suffix", "docs: back up docs/reference.md.bak", "docs/reference.md", false},
		{"a full stop after the path", "see docs/reference.md.", "docs/reference.md", true},
		{"a comma after the path", "see docs/reference.md, then docs/scorecard.md", "docs/reference.md", true},
		{"a text that does not hold the doc", "docs: fix docs/scorecard.md", "docs/reference.md", false},
		{"a doc with regexp metacharacters", "see docs/a+b(c).md for the flags", "docs/a+b(c).md", true},
		{"a text that is not the metacharactered doc", "see docs/ab(c)Xmd for the flags", "docs/a+b(c).md", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := names.namesDoc(tc.text, tc.doc); got != tc.want {
				t.Errorf("namesDoc(%q, %q) = %v, want %v", tc.text, tc.doc, got, tc.want)
			}
		})
	}
}

// TestNamesDocCompilesOncePerDoc drives namesDoc over many texts for two docs,
// counting each doc's pattern compiles through the seam: one compile per doc,
// however many texts ask, and each doc's pattern answers only its own texts.
func TestNamesDocCompilesOncePerDoc(t *testing.T) {
	t.Parallel()
	compiles := map[string]int{}
	names := newDocNameCache()
	names.compile = func(doc string) *regexp.Regexp {
		compiles[doc]++
		return compileDocName(doc)
	}
	// Each text holds the doc's literal path without naming it, so every call
	// reaches the pattern rather than the literal check's early return.
	for i := 0; i < 20; i++ {
		text := fmt.Sprintf("docs: touch old/docs/reference.md part %d", i)
		if names.namesDoc(text, "docs/reference.md") {
			t.Fatalf("namesDoc(%q) = true, want false", text)
		}
		text = fmt.Sprintf("docs: touch old/docs/scorecard.md part %d", i)
		if names.namesDoc(text, "docs/scorecard.md") {
			t.Fatalf("namesDoc(%q) = true, want false", text)
		}
	}
	for _, doc := range []string{"docs/reference.md", "docs/scorecard.md"} {
		if got := compiles[doc]; got != 1 {
			t.Errorf("%s compiled %d times, want 1", doc, got)
		}
	}
}

// TestDriftLabelNamesEachDocWithOneCompile drives Label for two docs over many
// landed beads, counting each doc's pattern compiles through the seam: one
// compile per doc, however many beads its window holds, and one doc's cached
// pattern does not answer the other's.
func TestDriftLabelNamesEachDocWithOneCompile(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	closedAt := driftAsk.Add(5 * day).Format(time.RFC3339Nano)
	var issues []*beads.Issue
	// Every bead but the last two holds one doc's text without naming it, so
	// each Label runs that doc's pattern for every bead before the naming one
	// is reached.
	for i := 0; i < 40; i++ {
		issues = append(issues, &beads.Issue{
			ID:          fmt.Sprintf("gt-%02d", i),
			Title:       fmt.Sprintf("docs: touch old/docs/reference.md part %d", i),
			Status:      statusClosed,
			CloseReason: "Landed on main (abc123)",
			ClosedAt:    closedAt,
		})
	}
	issues = append(issues,
		&beads.Issue{
			ID:          "gt-scorecard",
			Title:       "docs: fix docs/scorecard.md",
			Status:      statusClosed,
			CloseReason: "Landed on main (abc123)",
			ClosedAt:    closedAt,
		},
		&beads.Issue{
			ID:          "gt-named",
			Title:       "docs: refresh the drift section in docs/reference.md",
			Status:      statusClosed,
			CloseReason: "Landed on main (abc123)",
			ClosedAt:    closedAt,
		},
	)

	compiles := map[string]int{}
	a := newDriftAdapter("", fakeDriftBeads{byRig: map[string][]*beads.Issue{"gastown": issues}}, fakeDocs{},
		func() time.Time { return driftAsk.Add(40 * day) })
	a.names.compile = func(doc string) *regexp.Regexp {
		compiles[doc]++
		return compileDocName(doc)
	}

	docs := []string{"docs/reference.md", "docs/scorecard.md"}
	for i := 0; i < 10; i++ {
		for _, doc := range docs {
			lab, err := a.Label(context.Background(), driftItemFor(driftAsk, doc, "Drift battery", ""))
			if err != nil {
				t.Fatal(err)
			}
			if !lab.Labeled || !lab.Labels[qDocNowStale] || lab.Rule != ruleDriftBeadLanded {
				t.Fatalf("label for %s = %+v, want doc_now_stale under %q", doc, lab, ruleDriftBeadLanded)
			}
		}
	}
	for _, doc := range docs {
		if got := compiles[doc]; got != 1 {
			t.Errorf("%s compiled %d times, want 1", doc, got)
		}
	}
}

func TestDriftLabelKindMatchesTheEditsDiff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		added  string
		picked string
		want   bool
	}{
		{name: "a flag the edit names, picked as a flag", added: "+Pass `--recent` widens it.", picked: "flag", want: true},
		{name: "a flag the edit names, picked as a command", added: "+Pass `--recent` widens it.", picked: "command", want: false},
		{name: "a command phrase, picked as a command", added: "+Run gt jev scorecard now.", picked: "command", want: true},
		{name: "a snake_case key, picked as a config key", added: "+Set jev_drift to five.", picked: "config_key", want: true},
		{name: "a flag is not also a config key", added: "+Pass `--max-usd` to it.", picked: "config_key", want: false},
		{name: "prose names no identifier and is a behavior", added: "+The loop waits a while.", picked: "behavior", want: true},
		{name: "prose picked as none is wrong", added: "+The loop waits a while.", picked: "none", want: false},
		{name: "a flag edit picked as unclear is wrong", added: "+Pass `--recent` widens it.", picked: "unclear", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			patch := "@@ -5 +5 @@\n-Text.\n" + tc.added + "\n"
			a := newDriftAdapter("", &fakeDriftBeads{}, fakeDocs{edits: map[string][]docEdit{
				"docs/reference.md": {driftEditAt(driftAsk.Add(time.Hour), patch)},
			}}, func() time.Time { return driftAsk.Add(2 * time.Hour) })
			lab, err := a.Label(context.Background(), driftItem(driftAsk, "Drift battery", tc.picked))
			if err != nil {
				t.Fatal(err)
			}
			got, ok := lab.Labels[qDriftKind]
			if !ok {
				t.Fatalf("drift_kind unlabeled, rule %q", lab.Rule)
			}
			if got != tc.want {
				t.Errorf("drift_kind = %v, want %v", got, tc.want)
			}
			if !strings.Contains(lab.Rule, ruleDriftKindInEdit) {
				t.Errorf("rule = %q, want %q joined in", lab.Rule, ruleDriftKindInEdit)
			}
		})
	}
}

func TestDriftLabelKindIsUnlabeledWithoutAnEdit(t *testing.T) {
	t.Parallel()
	a := newDriftAdapter("", &fakeDriftBeads{}, fakeDocs{}, func() time.Time { return driftAsk.Add(40 * 24 * time.Hour) })
	lab, err := a.Label(context.Background(), driftItem(driftAsk, "Drift battery", "flag"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lab.Labels[qDriftKind]; ok {
		t.Errorf("drift_kind labeled with no edit to check it against: %+v", lab.Labels)
	}
}

func TestDriftSectionChange(t *testing.T) {
	t.Parallel()
	const before = "# Doc\n\n## Scorecard\n\nAlpha.\n\n## Drift battery\n\nBeta.\n"
	cases := []struct {
		name    string
		before  string
		patch   string
		heading string
		want    bool
	}{
		{name: "a body line of the section", before: before, patch: "@@ -9 +9 @@\n-Beta.\n+Beta now.\n", heading: "Drift battery", want: true},
		{name: "a line of the other section", before: before, patch: "@@ -5 +5 @@\n-Alpha.\n+Alpha now.\n", heading: "Drift battery", want: false},
		{name: "an insertion after the section's last line", before: before, patch: "@@ -6,0 +7 @@\n+Gamma.\n", heading: "Scorecard", want: true},
		{name: "an insertion just before the section's heading", before: before, patch: "@@ -6,0 +7 @@\n+Gamma.\n", heading: "Drift battery", want: false},
		{name: "an insertion after the section's heading", before: before, patch: "@@ -7,0 +8 @@\n+Gamma.\n", heading: "Drift battery", want: true},
		{name: "a deletion that reaches the section", before: before, patch: "@@ -7,2 +7,0 @@\n-## Drift battery\n-\n", heading: "Drift battery", want: true},
		{name: "a heading the edit adds", before: "", patch: "@@ -0,0 +1,3 @@\n+# Doc\n+\n+## Drift battery\n", heading: "Drift battery", want: true},
		{name: "a heading inside a code fence is body text", before: "## A\n```\n# B\n```\n", patch: "@@ -3 +3 @@\n-# B\n+# C\n", heading: "B", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, got := sectionChange(docEdit{Before: tc.before, Patch: tc.patch}, tc.heading)
			if got != tc.want {
				t.Errorf("touches %q = %v, want %v", tc.heading, got, tc.want)
			}
		})
	}
}

func TestDriftSectionSpansSkipFencedHeadings(t *testing.T) {
	t.Parallel()
	const text = "## A\n```\n# not a heading\n```\n## B\n"
	got := sectionSpans(text, "A")
	want := [][2]int{{1, 5}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("spans of A = %v, want %v", got, want)
	}
}

func TestDriftParseLinesReadsOneItemPerSection(t *testing.T) {
	t.Parallel()
	a := newDriftAdapter("", &fakeDriftBeads{}, fakeDocs{}, time.Now)
	line := `{"ts":"2026-09-02T00:00:00Z","bead":"gt-a","rig":"gastown","model":"m1","question_version":1,
		"sections":[
			{"doc":"docs/reference.md","heading":"Drift battery","answers":{
				"doc_now_stale":{"noul":0.9},
				"drift_kind":{"choice":"flag","probabilities":{"flag":0.8,"none":0.2}}}},
			{"doc":"docs/reference.md","heading":"Scorecard","answers":{"doc_now_stale":{"noul":0.1}}}
		]}`
	items, err := a.ParseLines([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want one per section (2)", len(items))
	}
	if items[0].ID == items[1].ID {
		t.Errorf("sections share item id %q", items[0].ID)
	}
	if items[0].Refs[refSection] != "Drift battery" || items[1].Refs[refSection] != "Scorecard" {
		t.Errorf("headings = %q, %q", items[0].Refs[refSection], items[1].Refs[refSection])
	}
	kind := items[0].Answers[qDriftKind]
	if kind.Positive != "flag" || kind.Choice != "flag" {
		t.Errorf("drift_kind = %+v, want the picked flag as its positive option", kind)
	}
	if items[0].Answers[qDocNowStale].Noul == nil {
		t.Errorf("doc_now_stale noul is missing: %+v", items[0].Answers)
	}
	if !a.since.Equal(time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("since = %v, want the line's ts", a.since)
	}
}

// TestDriftParseLinesOmitsNeedsNewSection checks that the question with no
// label rule never reaches the scorecard, while the two that have one stay.
func TestDriftParseLinesOmitsNeedsNewSection(t *testing.T) {
	t.Parallel()
	a := newDriftAdapter("", &fakeDriftBeads{}, fakeDocs{}, time.Now)
	line := `{"ts":"2026-09-02T00:00:00Z","bead":"gt-a","rig":"gastown","model":"m1","question_version":1,
		"sections":[{"doc":"docs/reference.md","heading":"Drift battery","answers":{
			"doc_now_stale":{"noul":0.9},
			"needs_new_section":{"noul":0.2},
			"drift_kind":{"choice":"flag","probabilities":{"flag":0.8,"none":0.2}}}}]}`
	items, err := a.ParseLines([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	answers := items[0].Answers
	if _, ok := answers[QuestionID(drift.QNeedsNewSection)]; ok {
		t.Errorf("needs_new_section is scored: %+v", answers)
	}
	if _, ok := answers[qDocNowStale]; !ok {
		t.Errorf("doc_now_stale is missing: %+v", answers)
	}
	if _, ok := answers[qDriftKind]; !ok {
		t.Errorf("drift_kind is missing: %+v", answers)
	}
}

func TestDriftParseLinesSkipsWholeBadLines(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"another question set": `{"ts":"2026-09-02T00:00:00Z","bead":"gt-a","rig":"gastown","question_version":99,"sections":[]}`,
		"a doc outside docs/":  `{"ts":"2026-09-02T00:00:00Z","bead":"gt-a","rig":"gastown","question_version":1,"sections":[{"doc":"mayor/rig/x.md","heading":"x","answers":{}}]}`,
		"a doc with a range":   `{"ts":"2026-09-02T00:00:00Z","bead":"gt-a","rig":"gastown","question_version":1,"sections":[{"doc":"docs/../x.md","heading":"x","answers":{}}]}`,
		"no bead":              `{"ts":"2026-09-02T00:00:00Z","rig":"gastown","question_version":1,"sections":[]}`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := newDriftAdapter("", &fakeDriftBeads{}, fakeDocs{}, time.Now)
			if items, err := a.ParseLines([]byte(line)); err == nil {
				t.Errorf("parsed %d items, want the line refused", len(items))
			}
		})
	}
}

func TestDriftBatteryIsRegistered(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	a, err := Open(DriftBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	if a.Battery() != DriftBattery {
		t.Errorf("battery = %q, want %q", a.Battery(), DriftBattery)
	}
	if got := a.LogPath(); got != drift.LogPath(town) {
		t.Errorf("log = %q, want the drift log %q", got, drift.LogPath(town))
	}
	if _, ok := a.(Expander); !ok {
		t.Error("the drift adapter does not read one item per section")
	}
}

// A top-of-file insertion (start 0) sits above line 1, so it touches the
// preamble span that starts at line 1 and no span further down.
func TestOverlapsAnyTopOfFileInsertion(t *testing.T) {
	t.Parallel()
	if !overlapsAny([][2]int{{1, 5}}, 0, 0) {
		t.Error("a top-of-file insertion did not touch the preamble span")
	}
	if overlapsAny([][2]int{{3, 5}}, 0, 0) {
		t.Error("a top-of-file insertion touched a span that starts below line 1")
	}
}

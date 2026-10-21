package scorecard

import (
	"context"
	"fmt"
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

// fakeDocs serves each doc's edits from memory.
type fakeDocs struct {
	edits map[string][]docEdit
}

func (f fakeDocs) Edits(_ context.Context, doc string) ([]docEdit, error) {
	return f.edits[doc], nil
}

// fakeDriftBeads serves each rig's beads from memory. It returns every bead, so
// the window is the adapter's to apply, as it is for the real store's listing.
type fakeDriftBeads struct {
	byRig map[string][]*beads.Issue
}

func (f fakeDriftBeads) List(rig string, _ beads.ListOptions) ([]*beads.Issue, error) {
	return f.byRig[rig], nil
}

// driftItem is the item a drift line makes for one section. An empty kind
// leaves drift_kind unanswered.
func driftItem(ask time.Time, heading, kind string) Item {
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
			refDoc:     "docs/reference.md",
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

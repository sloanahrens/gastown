package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/stale"
)

var updateStaleGolden = flag.Bool("update-stale-golden", false, "rewrite testdata/stale.golden")

// fakeStaleBeads returns scripted beads by id and reports a missing id as not
// found, as the production reader does.
type fakeStaleBeads struct {
	byID map[string]*beads.Issue
}

func (f *fakeStaleBeads) Show(_, bead string) (*beads.Issue, error) {
	issue, ok := f.byID[bead]
	if !ok {
		return nil, beads.ErrNotFound
	}
	return issue, nil
}

func newFakeStaleAdapter(issues map[string]*beads.Issue) *staleAdapter {
	return &staleAdapter{
		issues: &fakeStaleBeads{byID: issues},
		lines:  map[string]stale.Line{},
		cache:  map[string]*beads.Issue{},
	}
}

// staleLine is a logged read of bead at priority, asked at ts, with answers.
func staleLine(bead string, priority int, ts time.Time, answers map[string]stale.Answer) stale.Line {
	return stale.Line{
		TS:              ts,
		Rig:             "gastown",
		Bead:            bead,
		Type:            "task",
		Priority:        priority,
		UpdatedAt:       ts.Add(-time.Hour),
		Answers:         answers,
		QuestionVersion: stale.Version,
		Model:           "jev-test",
	}
}

func nounAnswer(p float64) stale.Answer { return stale.Answer{Noul: &p} }

func scoreAnswer(level, confidence float64) stale.Answer {
	return stale.Answer{Score: &level, Confidence: &confidence}
}

// labelStale parses line the way Build does and labels it.
func labelStale(t *testing.T, a *staleAdapter, line stale.Line) (Labeling, error) {
	t.Helper()
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	item, err := a.Parse(raw)
	if err != nil {
		t.Fatalf("parsing the line: %v", err)
	}
	return a.Label(context.Background(), item)
}

func TestStaleLabelRules(t *testing.T) {
	t.Parallel()
	ask := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	answers := map[string]stale.Answer{
		stale.QObsolete: nounAnswer(0.5),
	}
	cases := []struct {
		name    string
		askedAt int // priority when the bead was asked about
		bead    *beads.Issue
		missing bool // the bead is not in the store
		labeled bool
		labels  map[QuestionID]bool
		rule    string
	}{
		{
			name:    "a bead closed as wontfix is obsolete",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "wontfix", Priority: 2},
			labeled: true,
			labels:  map[QuestionID]bool{qObsolete: true},
			rule:    ruleStaleClosedObsolete,
		},
		{
			name:    "a bead closed as obsolete is obsolete",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "Obsolete: the rig was retired", Priority: 2},
			labeled: true,
			labels:  map[QuestionID]bool{qObsolete: true},
			rule:    ruleStaleClosedObsolete,
		},
		{
			name:    "a bead closed as no longer needed is obsolete",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "no longer needed after the rewrite", Priority: 2},
			labeled: true,
			labels:  map[QuestionID]bool{qObsolete: true},
			rule:    ruleStaleClosedObsolete,
		},
		{
			name:    "a bead closed as superseded is superseded",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "superseded by gt-n1", Priority: 2},
			labeled: true,
			labels:  map[QuestionID]bool{qSuperseded: true},
			rule:    ruleStaleClosedSuperseded,
		},
		{
			name:    "a bead closed as a duplicate is superseded",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "duplicate of gt-d1", Priority: 2},
			labeled: true,
			labels:  map[QuestionID]bool{qSuperseded: true},
			rule:    ruleStaleClosedSuperseded,
		},
		{
			name:    "a landed bead is worth doing and not obsolete or superseded",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "landed", Priority: 2},
			labeled: true,
			labels:  map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false},
			rule:    ruleStaleLanded,
		},
		{
			name:    "the landing record's close reason is a landing too",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "Landed on main\ntarget_branch: main\ncommit_sha: abc", Priority: 2},
			labeled: true,
			labels:  map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false},
			rule:    ruleStaleLanded,
		},
		{
			name:    "a superseding close quoting a landing record is superseded",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "superseded by gt-n2\nlanded_commit: abc123", Priority: 2},
			labeled: true,
			labels:  map[QuestionID]bool{qSuperseded: true},
			rule:    ruleStaleClosedSuperseded,
		},
		{
			name:    "a landing that mentions an obsolete copy is still landed",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "Landed on main\nnote: the obsolete copy went too", Priority: 2},
			labeled: true,
			labels:  map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false},
			rule:    ruleStaleLanded,
		},
		{
			name:    "an obsolete close that quotes a landing record is obsolete",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "obsolete: the rig was retired\nlanded_commit: abc123", Priority: 2},
			labeled: true,
			labels:  map[QuestionID]bool{qObsolete: true},
			rule:    ruleStaleClosedObsolete,
		},
		{
			name:    "a bead closed for another reason is unlabeled",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "done by hand", Priority: 2},
		},
		{
			name:    "an open bead with its priority unchanged is unlabeled",
			askedAt: 2,
			bead:    &beads.Issue{Status: "open", Priority: 2},
		},
		{
			name:    "a bead raised since the ask sets priority_fit true",
			askedAt: 3,
			bead:    &beads.Issue{Status: "open", Priority: 1},
			labeled: true,
			labels:  map[QuestionID]bool{qPriorityFit: true},
			rule:    ruleStalePriorityRaised,
		},
		{
			name:    "a bead lowered since the ask sets priority_fit false",
			askedAt: 2,
			bead:    &beads.Issue{Status: "open", Priority: 4},
			labeled: true,
			labels:  map[QuestionID]bool{qPriorityFit: false},
			rule:    ruleStalePriorityLowered,
		},
		{
			name:    "a landed bead raised since the ask gets both rules",
			askedAt: 2,
			bead:    &beads.Issue{Status: "closed", CloseReason: "landed", Priority: 1},
			labeled: true,
			labels:  map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false, qPriorityFit: true},
			rule:    ruleStaleLanded + "+" + ruleStalePriorityRaised,
		},
		{
			name:    "a bead that no longer exists is unlabeled",
			askedAt: 2,
			missing: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issues := map[string]*beads.Issue{}
			if !tc.missing {
				issues["gt-s1"] = tc.bead
			}
			a := newFakeStaleAdapter(issues)
			lab, err := labelStale(t, a, staleLine("gt-s1", tc.askedAt, ask, answers))
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled != tc.labeled {
				t.Fatalf("labeled = %v, want %v", lab.Labeled, tc.labeled)
			}
			if !tc.labeled {
				if lab.Labels != nil || lab.Rule != "" {
					t.Errorf("unlabeled line carries labels %v and rule %q", lab.Labels, lab.Rule)
				}
				return
			}
			if !maps.Equal(lab.Labels, tc.labels) {
				t.Errorf("labels = %v, want %v", lab.Labels, tc.labels)
			}
			if lab.Rule != tc.rule {
				t.Errorf("rule = %q, want %q", lab.Rule, tc.rule)
			}
		})
	}
}

// TestStaleNeedsReshapingRule checks the reshape rule: a landed bead whose
// description changed after the ask sets needs_reshaping true, and any other
// line stays unlabeled for it. The hash is taken of the description at the ask.
func TestStaleNeedsReshapingRule(t *testing.T) {
	t.Parallel()
	ask := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	answers := map[string]stale.Answer{
		stale.QNeedsReshape: nounAnswer(0.6),
	}
	const askedDesc = "Add the flag to gt done."
	const reshapedDesc = "Add the flag to gt done, and the docs."
	cases := []struct {
		name     string
		descHash string // the hash the line carries; empty is a pre-field line
		bead     *beads.Issue
		labeled  bool
		labels   map[QuestionID]bool
		rule     string
	}{
		{
			name:     "a landed bead whose description changed sets needs_reshaping true",
			descHash: stale.HashDescription(askedDesc),
			bead:     &beads.Issue{Status: "closed", CloseReason: "landed", Priority: 2, Description: reshapedDesc},
			labeled:  true,
			labels:   map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false, qNeedsReshape: true},
			rule:     ruleStaleLanded + "+" + ruleStaleReshaped,
		},
		{
			name:     "a landed bead whose description did not change gets no needs_reshaping label",
			descHash: stale.HashDescription(askedDesc),
			bead:     &beads.Issue{Status: "closed", CloseReason: "landed", Priority: 2, Description: askedDesc},
			labeled:  true,
			labels:   map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false},
			rule:     ruleStaleLanded,
		},
		{
			name:     "a line logged before the hash field exists gets no needs_reshaping label",
			descHash: "",
			bead:     &beads.Issue{Status: "closed", CloseReason: "landed", Priority: 2, Description: reshapedDesc},
			labeled:  true,
			labels:   map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false},
			rule:     ruleStaleLanded,
		},
		{
			name:     "a landed bead whose description is gone now gets no needs_reshaping label",
			descHash: stale.HashDescription(askedDesc),
			bead:     &beads.Issue{Status: "closed", CloseReason: "landed", Priority: 2},
			labeled:  true,
			labels:   map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false},
			rule:     ruleStaleLanded,
		},
		{
			name:     "a landed bead whose description is only whitespace now gets no needs_reshaping label",
			descHash: stale.HashDescription(askedDesc),
			bead:     &beads.Issue{Status: "closed", CloseReason: "landed", Priority: 2, Description: " \n\t "},
			labeled:  true,
			labels:   map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false},
			rule:     ruleStaleLanded,
		},
		{
			name:     "a landed bead asked about bare and described since gets needs_reshaping true",
			descHash: stale.HashDescription(""),
			bead:     &beads.Issue{Status: "closed", CloseReason: "landed", Priority: 2, Description: reshapedDesc},
			labeled:  true,
			labels:   map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false, qNeedsReshape: true},
			rule:     ruleStaleLanded + "+" + ruleStaleReshaped,
		},
		{
			name:     "a landed bead bare at the ask and now gets no needs_reshaping label",
			descHash: stale.HashDescription(""),
			bead:     &beads.Issue{Status: "closed", CloseReason: "landed", Priority: 2},
			labeled:  true,
			labels:   map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false},
			rule:     ruleStaleLanded,
		},
		{
			name:     "a changed description on a bead still open is unlabeled",
			descHash: stale.HashDescription(askedDesc),
			bead:     &beads.Issue{Status: "open", Priority: 2, Description: reshapedDesc},
		},
		{
			name:     "a changed description on a bead closed for another reason is unlabeled",
			descHash: stale.HashDescription(askedDesc),
			bead:     &beads.Issue{Status: "closed", CloseReason: "done by hand", Priority: 2, Description: reshapedDesc},
		},
		{
			name:     "a landed bead whose description changed and was raised gets all three rules",
			descHash: stale.HashDescription(askedDesc),
			bead:     &beads.Issue{Status: "closed", CloseReason: "landed", Priority: 1, Description: reshapedDesc},
			labeled:  true,
			labels:   map[QuestionID]bool{qWorthDoing: true, qObsolete: false, qSuperseded: false, qNeedsReshape: true, qPriorityFit: true},
			rule:     ruleStaleLanded + "+" + ruleStaleReshaped + "+" + ruleStalePriorityRaised,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newFakeStaleAdapter(map[string]*beads.Issue{"gt-s1": tc.bead})
			line := staleLine("gt-s1", 2, ask, answers)
			line.DescHash = tc.descHash
			lab, err := labelStale(t, a, line)
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled != tc.labeled {
				t.Fatalf("labeled = %v, want %v", lab.Labeled, tc.labeled)
			}
			if !tc.labeled {
				if lab.Labels != nil || lab.Rule != "" {
					t.Errorf("unlabeled line carries labels %v and rule %q", lab.Labels, lab.Rule)
				}
				return
			}
			if !maps.Equal(lab.Labels, tc.labels) {
				t.Errorf("labels = %v, want %v", lab.Labels, tc.labels)
			}
			if lab.Rule != tc.rule {
				t.Errorf("rule = %q, want %q", lab.Rule, tc.rule)
			}
		})
	}
}

// TestTownNotesReadsAMissingRigAsNotFound checks that a rig with no beads
// directory reads as not found, for a bead and for a listing, and opens no
// store, so the read never falls back to whatever beads database the working
// directory holds. A listing that read as empty would let a rule label as if
// it had looked.
func TestTownNotesReadsAMissingRigAsNotFound(t *testing.T) {
	t.Parallel()
	tn := &townNotes{town: t.TempDir(), stores: map[string]*beads.Beads{}}
	_, err := tn.Show("gastown", "gt-gone")
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Show of a rig with no beads directory = %v, want not found", err)
	}
	_, err = tn.List("gastown", beads.ListOptions{})
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("List of a rig with no beads directory = %v, want not found", err)
	}
	if len(tn.stores) != 0 {
		t.Errorf("a rig with no beads directory opened %d store(s)", len(tn.stores))
	}
}

func TestStaleParseSkipsWhatCannotBeCompared(t *testing.T) {
	t.Parallel()
	ask := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	a := newFakeStaleAdapter(nil)

	asked := staleLine("gt-p1", 2, ask, map[string]stale.Answer{
		stale.QObsolete:    nounAnswer(0.8),
		stale.QPriorityFit: scoreAnswer(3, 0.6),
	})
	raw, err := json.Marshal(asked)
	if err != nil {
		t.Fatal(err)
	}
	item, err := a.Parse(raw)
	if err != nil {
		t.Fatalf("a line with answers failed to parse: %v", err)
	}
	if item.ID != "gastown/gt-p1@"+ask.Format(time.RFC3339Nano) {
		t.Errorf("item id = %q", item.ID)
	}
	if got := item.Answers[QuestionID(stale.QObsolete)]; got.Noul == nil || *got.Noul != 0.8 {
		t.Errorf("obsolete answer = %+v, want noul 0.8", got)
	}
	if got := item.Answers[QuestionID(stale.QPriorityFit)]; got.Score == nil || *got.Score != 3 {
		t.Errorf("priority_fit answer = %+v, want score 3", got)
	}

	unasked := staleLine("gt-p2", 2, ask, nil)
	unasked.Reason = stale.ReasonAskFailed
	oldVersion := staleLine("gt-p3", 2, ask, map[string]stale.Answer{stale.QObsolete: nounAnswer(0.1)})
	oldVersion.QuestionVersion = stale.Version + 1
	rigLine := staleLine("gt-rig-gastown", 2, ask, map[string]stale.Answer{stale.QObsolete: nounAnswer(0.1)})
	rigLine.Type = "rig"
	moleculeLine := staleLine("gt-37k", 2, ask, map[string]stale.Answer{stale.QObsolete: nounAnswer(0.1)})
	moleculeLine.Type = "molecule"
	untypedLine := staleLine("gt-p4", 2, ask, map[string]stale.Answer{stale.QObsolete: nounAnswer(0.1)})
	untypedLine.Type = ""

	for name, line := range map[string]stale.Line{
		"a line whose call failed":         unasked,
		"a line from another question set": oldVersion,
		"a line about a rig bead":          rigLine,
		"a line about a molecule bead":     moleculeLine,
		"a line with no type":              untypedLine,
	} {
		raw, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Parse(raw); err == nil {
			t.Errorf("%s parsed, want it skipped", name)
		}
	}
}

func TestStaleBatteryIsRegistered(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	a, err := Open(StaleBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	if a.Battery() != StaleBattery {
		t.Errorf("battery = %q, want %q", a.Battery(), StaleBattery)
	}
	if got := a.LogPath(); got != stale.LogPath(town) {
		t.Errorf("log = %q, want the stale log %q", got, stale.LogPath(town))
	}
}

// staleLogLines are the fixture log's lines. Each names a bead in the fake
// store below, and one line the pass could not ask about.
func staleLogLines(t *testing.T) []string {
	t.Helper()
	ask := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	lines := []stale.Line{
		staleLine("gt-g1", 2, ask, map[string]stale.Answer{
			stale.QObsolete:     nounAnswer(0.9),
			stale.QSuperseded:   nounAnswer(0.1),
			stale.QWorthDoing:   nounAnswer(0.2),
			stale.QNeedsReshape: nounAnswer(0.3),
			stale.QPriorityFit:  scoreAnswer(2, 0.7),
		}),
		staleLine("gt-g2", 3, ask, map[string]stale.Answer{
			stale.QObsolete:     nounAnswer(0.2),
			stale.QSuperseded:   nounAnswer(0.8),
			stale.QWorthDoing:   nounAnswer(0.3),
			stale.QNeedsReshape: nounAnswer(0.1),
			stale.QPriorityFit:  scoreAnswer(2, 0.5),
		}),
		staleLine("gt-g3", 2, ask, map[string]stale.Answer{
			stale.QObsolete:     nounAnswer(0.1),
			stale.QSuperseded:   nounAnswer(0.1),
			stale.QWorthDoing:   nounAnswer(0.9),
			stale.QNeedsReshape: nounAnswer(0.4),
			stale.QPriorityFit:  scoreAnswer(3, 0.8),
		}),
		staleLine("gt-g4", 2, ask, map[string]stale.Answer{
			stale.QObsolete:    nounAnswer(0.4),
			stale.QWorthDoing:  nounAnswer(0.6),
			stale.QPriorityFit: scoreAnswer(2, 0.9),
		}),
		staleLine("gt-g5", 3, ask, map[string]stale.Answer{
			stale.QWorthDoing:  nounAnswer(0.5),
			stale.QPriorityFit: scoreAnswer(0, 0.6),
		}),
	}
	unasked := staleLine("gt-g6", 2, ask, nil)
	unasked.Reason = stale.ReasonAskFailed

	out := make([]string, 0, len(lines)+1)
	for _, l := range append(lines, unasked) {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(raw))
	}
	return out
}

// staleFixtureBeads is the store the fixture log's beads read from.
func staleFixtureBeads() map[string]*beads.Issue {
	return map[string]*beads.Issue{
		"gt-g1": {Status: "closed", CloseReason: "wontfix", Priority: 2},
		"gt-g2": {Status: "closed", CloseReason: "superseded by gt-n1", Priority: 3},
		"gt-g3": {Status: "closed", CloseReason: "landed", Priority: 1},
		"gt-g4": {Status: "open", Priority: 2},
		"gt-g5": {Status: "open", Priority: 4},
	}
}

// TestStaleScorecardPrintsPerQuestionBlocks runs the scorecard over a fixture
// log and fake beads, and compares the text report with its golden file.
func TestStaleScorecardPrintsPerQuestionBlocks(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logPath := stale.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(staleLogLines(t), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newFakeStaleAdapter(staleFixtureBeads())
	a.log = logPath

	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 5 || rep.LinesSkipped != 1 {
		t.Fatalf("items %d skipped %d, want 5 and 1", rep.Items, rep.LinesSkipped)
	}

	var out strings.Builder
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	goldenPath := filepath.Join("testdata", "stale.golden")
	if *updateStaleGolden {
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
		t.Errorf("output differs from %s (rewrite with -update-stale-golden):\n%s\nwant:\n%s", goldenPath, got, want)
	}
}

// A log the pass wrote before it read only work beads holds lines about rig and
// molecule beads. The report discards them: they are skipped lines, not items,
// and no question is scored on them.
func TestStaleScorecardDiscardsLinesAboutNonWorkBeads(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logPath := stale.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	ask := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ans := map[string]stale.Answer{stale.QObsolete: nounAnswer(0.8)}
	work := staleLine("gt-g4", 2, ask, ans)
	rig := staleLine("gt-rig-gastown", 2, ask, ans)
	rig.Type = "rig"
	molecule := staleLine("gt-37k", 2, ask, ans)
	molecule.Type = "molecule"
	var lines []string
	for _, l := range []stale.Line{rig, work, molecule} {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(raw))
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newFakeStaleAdapter(staleFixtureBeads())
	a.log = logPath

	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 1 || rep.LinesSkipped != 2 {
		t.Fatalf("items %d skipped %d, want 1 and 2", rep.Items, rep.LinesSkipped)
	}
	// The skipped lines are not items: only the work bead's line is reported.
	if len(rep.Lines) != 1 || rep.Lines[0].ID != "gastown/gt-g4@"+ask.Format(time.RFC3339Nano) {
		t.Errorf("lines %+v, want only the work bead's line", rep.Lines)
	}
	// All three lines carry the same answer, so a skipped line reaching a
	// question would show up as extra logged lines.
	if got := question(t, rep, stale.QObsolete).Logged; got != 1 {
		t.Errorf("obsolete logged %d, want 1: skipped lines are not scored", got)
	}
}

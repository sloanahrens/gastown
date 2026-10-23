package scorecard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	jevattention "github.com/steveyegge/gastown/internal/jev/attention"
	"github.com/steveyegge/gastown/internal/jev/stale"
)

var updateAttentionScorecardGolden = flag.Bool("update-attention-scorecard-golden", false, "rewrite testdata/attention_scorecard.golden")

// attentionBase is when the first test item was raised. Each case is raised an
// hour after the one before it, so no two lives overlap.
var attentionBase = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// routineQuestionSet is the last attention question set that asked
// routine_action_fixes.
const routineQuestionSet = 1

// attentionCase is one item life in the test town, and what the adapter must
// say about it. A zero clear is a life still standing.
type attentionCase struct {
	name  string
	kind  attention.Kind
	key   string
	bead  string
	sha   string
	clear time.Duration
	// ack is a disposition an acks-history line records one minute after the
	// raise. legacyAck writes an acks-history line with no disposition.
	ack       attention.Disposition
	legacyAck bool
	// issue is the bead the case names, nil when the bead does not exist.
	issue *beads.Issue
	// noEvents leaves the life out of the queue's events, so only the battery
	// line says it existed.
	noEvents bool
	// notLive writes the battery line as one asked after the item cleared,
	// which carries no bead.
	notLive bool
	// oldSet writes the line under the last question set that asked
	// routine_action_fixes, as an older line in the log was. A rule that
	// labels routine_action_fixes needs that answer to score against.
	oldSet bool
	// want is the labels the adapter must set, nil when it must leave the item
	// unlabeled; wantRule names the rule that sets them.
	want     map[string]bool
	wantRule string
}

func escalationBead(id, closedBy string) *beads.Issue {
	return &beads.Issue{
		ID:          id,
		Status:      "closed",
		CloseReason: "handled",
		Description: "closed_by: " + closedBy + "\nclosed_reason: handled",
	}
}

func attentionCases() []attentionCase {
	const producer = "gastown/polecats/furiosa"
	return []attentionCase{
		{name: "escalation closed by the overseer needs a human", kind: attention.KindEscalation,
			key: "esc:hq-1", bead: "hq-1", clear: 2 * time.Hour, issue: escalationBead("hq-1", "overseer"),
			want: map[string]bool{"needs_human": true}, wantRule: "escalation:closed-by-person"},
		{name: "escalation cleared by a producer clears on its own", kind: attention.KindEscalation,
			key: "esc:hq-2", bead: "hq-2", clear: 2 * time.Hour, issue: escalationBead("hq-2", producer),
			want: map[string]bool{"clears_on_its_own": true}, wantRule: "escalation:cleared-by-producer"},
		{name: "a needed-human disposition beats the producer mechanism", kind: attention.KindEscalation,
			key: "esc:hq-3", bead: "hq-3", clear: 2 * time.Hour, ack: attention.DispositionNeededHuman,
			issue: escalationBead("hq-3", producer),
			want:  map[string]bool{"needs_human": true}, wantRule: "ack:needed-human"},
		{name: "a noise disposition labels a still-open item", kind: attention.KindEscalation,
			key: "esc:hq-4", bead: "hq-4", ack: attention.DispositionNoise,
			issue: &beads.Issue{ID: "hq-4", Status: "open"},
			want:  map[string]bool{"needs_human": false, "clears_on_its_own": true}, wantRule: "ack:noise"},
		{name: "a fixed-by-overseer disposition labels a routine fix", kind: attention.KindRejectedTwice,
			key: "rework:gt-w5", bead: "gt-w5", clear: 3 * time.Hour, ack: attention.DispositionFixedByOverseer,
			issue: &beads.Issue{ID: "gt-w5", Status: "closed", CloseReason: "Landed on main"}, oldSet: true,
			want: map[string]bool{"routine_action_fixes": true}, wantRule: "ack:fixed-by-overseer"},
		{name: "a risk-path FAIL needs a human", kind: attention.KindRiskPath,
			key: "risk:gt-r6:abc1234", bead: "gt-r6", sha: "abc1234", clear: time.Hour,
			issue: &beads.Issue{ID: "gt-r6", Status: "open", Notes: "OVERSEER REVIEW abc1234 FAIL findings=gt-f6"},
			want:  map[string]bool{"needs_human": true}, wantRule: "risk-path:review-fail"},
		{name: "a risk-path PASS does not need a human", kind: attention.KindRiskPath,
			key: "risk:gt-r7:def5678", bead: "gt-r7", sha: "def5678", clear: time.Hour,
			issue: &beads.Issue{ID: "gt-r7", Status: "closed", Notes: "OVERSEER REVIEW def5678 PASS"},
			want:  map[string]bool{"needs_human": false}, wantRule: "risk-path:review-pass"},
		{name: "a risk-path WAIVED is left unlabeled", kind: attention.KindRiskPath,
			key: "risk:gt-r8:0123abc", bead: "gt-r8", sha: "0123abc", clear: time.Hour,
			issue: &beads.Issue{ID: "gt-r8", Status: "closed", Notes: "OVERSEER REVIEW 0123abc WAIVED: reviewed upstream"}},
		{name: "a superseded rework needs a human", kind: attention.KindRejectedTwice,
			key: "rework:gt-w9", bead: "gt-w9", clear: 3 * time.Hour,
			issue: &beads.Issue{ID: "gt-w9", Status: "closed", CloseReason: "superseded by gt-w10"},
			want:  map[string]bool{"needs_human": true}, wantRule: "rejected-twice:superseded"},
		{name: "a rework landed from a polecat branch is routine", kind: attention.KindRejectedTwice,
			key: "rework:gt-w11", bead: "gt-w11", clear: 3 * time.Hour, oldSet: true,
			issue: &beads.Issue{ID: "gt-w11", Status: "closed", CloseReason: "Landed on main\ntarget_branch: main",
				Notes: "LANDING RECORD\nlanded_commit: abc\nbranch: polecat/furiosa/gt-w11+x9"},
			want: map[string]bool{"routine_action_fixes": true}, wantRule: "rejected-twice:landed-routine"},
		{name: "a rework landed from a crew branch is left unlabeled", kind: attention.KindRejectedTwice,
			key: "rework:gt-w12", bead: "gt-w12", clear: 3 * time.Hour,
			issue: &beads.Issue{ID: "gt-w12", Status: "closed", CloseReason: "Landed on main",
				Notes: "LANDING RECORD\nbranch: crew/sloan/fix-x"}},
		{name: "a quiet item cleared inside 15 minutes clears on its own", kind: attention.KindHostMem,
			key: "host-mem:early", clear: 10 * time.Minute,
			want: map[string]bool{"clears_on_its_own": true}, wantRule: "cleared-within-15m"},
		{name: "a quiet item cleared after 15 minutes is left unlabeled", kind: attention.KindHostMem,
			key: "host-mem:late", clear: 20 * time.Minute},
		{name: "a legacy ack hides the self-clear window", kind: attention.KindHostMem,
			key: "host-mem:legacy", clear: 10 * time.Minute, legacyAck: true},
		{name: "a standing item with no disposition is left unlabeled", kind: attention.KindHostMem,
			key: "host-mem:open"},
		{name: "a bead with a note is not quiet", kind: attention.KindEscalation,
			key: "esc:hq-16", bead: "hq-16", clear: 5 * time.Minute,
			issue: &beads.Issue{ID: "hq-16", Status: "closed", Notes: "looked at it"}},
		{name: "an escalation whose bead is gone is left unlabeled", kind: attention.KindEscalation,
			key: "esc:hq-17", bead: "hq-17", clear: 5 * time.Minute},
		{name: "a line written after the item cleared has no bead and is left unlabeled", kind: attention.KindEscalation,
			key: "esc:hq-18", clear: 5 * time.Minute, notLive: true},
		{name: "a superseded reason is matched whatever its case", kind: attention.KindRejectedTwice,
			key: "rework:gt-w19", bead: "gt-w19", clear: 3 * time.Hour,
			issue: &beads.Issue{ID: "gt-w19", Status: "closed", CloseReason: "Superseded by gt-w20"},
			want:  map[string]bool{"needs_human": true}, wantRule: "rejected-twice:superseded"},
		{name: "a landed reason is matched whatever its case", kind: attention.KindRejectedTwice,
			key: "rework:gt-w21", bead: "gt-w21", clear: 3 * time.Hour, oldSet: true,
			issue: &beads.Issue{ID: "gt-w21", Status: "closed", CloseReason: "landed on main",
				Notes: "LANDING RECORD\nbranch: polecat/furiosa/gt-w21+x9"},
			want: map[string]bool{"routine_action_fixes": true}, wantRule: "rejected-twice:landed-routine"},
		{name: "a life the queue no longer holds is left unlabeled", kind: attention.KindHostMem,
			key: "host-mem:gone", clear: time.Minute, noEvents: true},
	}
}

// writeAttentionTown writes the queue files and the battery log the cases
// describe under town: events, acks history and one battery line per case.
func writeAttentionTown(t *testing.T, town string, cases []attentionCase) {
	t.Helper()
	if err := os.MkdirAll(attention.Dir(town), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(jevattention.LogPath(town)), 0o755); err != nil {
		t.Fatal(err)
	}

	var events []attention.Event
	var history []attention.AckRecord
	var lines []jevattention.Line
	for i, c := range cases {
		raised := attentionBase.Add(time.Duration(i) * time.Hour)
		if !c.noEvents {
			events = append(events, attention.Event{TS: raised, Class: c.kind, Severity: attention.SeverityHigh,
				Text: c.key, Key: c.key, State: attention.EventNew})
			if c.clear > 0 {
				events = append(events, attention.Event{TS: raised.Add(c.clear), Class: c.kind,
					Severity: attention.SeverityHigh, Text: c.key, Key: c.key, State: attention.EventCleared})
			}
		}
		if c.ack != "" {
			history = append(history, attention.AckRecord{TS: raised.Add(time.Minute), Key: c.key, Kind: c.kind, Disposition: c.ack})
		}
		if c.legacyAck {
			history = append(history, attention.AckRecord{TS: raised.Add(time.Minute), Key: c.key, Kind: c.kind})
		}
		line := jevattention.Line{
			TS: raised.Add(2 * time.Minute), Key: c.key, Kind: c.kind, Bead: c.bead, SHA: c.sha,
			FirstSeen: raised, Live: !c.notLive, QuestionVersion: jevattention.QuestionVersion, Model: "test-model",
			Answers: &jevattention.Answers{
				NeedsHuman: 0.7, ClearsOnItsOwn: 0.2, RepeatsOpenItem: 0,
				Urgency: jevattention.Urgency{Score: 1, Confidence: 0.8},
			},
		}
		if c.oldSet {
			line.QuestionVersion = routineQuestionSet
			line.Answers.RoutineActionFixes = ptr(0.1)
		}
		lines = append(lines, line)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].TS.Before(events[j].TS) })

	writeEvents(t, town, events)
	writeJSONLines(t, attention.AcksHistoryPath(town), history)
	writeJSONLines(t, jevattention.LogPath(town), lines)
}

func writeEvents(t *testing.T, town string, events []attention.Event) {
	t.Helper()
	if err := attention.AppendEvents(town, events); err != nil {
		t.Fatal(err)
	}
}

func writeJSONLines[T any](t *testing.T, path string, rows []T) {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeBeads serves the test beads. A bead missing from issues is not found.
type fakeBeads struct {
	issues map[string]*beads.Issue
	err    error
}

func (f fakeBeads) Show(id string) (*beads.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	if is, ok := f.issues[id]; ok {
		return is, nil
	}
	return nil, fmt.Errorf("%w: %s", beads.ErrNotFound, id)
}

func beadsOf(cases []attentionCase) fakeBeads {
	issues := map[string]*beads.Issue{}
	for _, c := range cases {
		if c.bead != "" && c.issue != nil {
			issues[c.bead] = c.issue
		}
	}
	return fakeBeads{issues: issues}
}

func newAttentionTestAdapter(town string, b beadShower) *attentionAdapter {
	return &attentionAdapter{town: town, beads: b, lines: map[string]jevattention.Line{}}
}

// caseID is the battery line ID of case i, the one Parse gives it.
func caseID(c attentionCase, i int) string {
	raised := attentionBase.Add(time.Duration(i) * time.Hour)
	return attentionID(jevattention.Line{Key: c.key, FirstSeen: raised})
}

func sameLabels(got, want map[string]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			return false
		}
	}
	return true
}

func TestAttentionLabelRules(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cases := attentionCases()
	writeAttentionTown(t, town, cases)

	rep, err := Build(context.Background(), newAttentionTestAdapter(town, beadsOf(cases)), time.Time{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.LinesSkipped != 0 || len(rep.Lines) != len(cases) {
		t.Fatalf("parsed %d lines, skipped %d; want %d and 0", len(rep.Lines), rep.LinesSkipped, len(cases))
	}
	byID := map[string]LineReport{}
	for _, lr := range rep.Lines {
		byID[lr.ID] = lr
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lr, ok := byID[caseID(c, i)]
			if !ok {
				t.Fatalf("no line for %s", c.key)
			}
			if c.want == nil {
				if lr.Labeled || lr.Rule != "" {
					t.Fatalf("labeled %v by %q, want unlabeled", lr.Labels, lr.Rule)
				}
				return
			}
			if !lr.Labeled {
				t.Fatalf("unlabeled, want %v by %s", c.want, c.wantRule)
			}
			if lr.Rule != c.wantRule {
				t.Errorf("rule %q, want %q", lr.Rule, c.wantRule)
			}
			if !sameLabels(lr.Labels, c.want) {
				t.Errorf("labels %v, want %v", lr.Labels, c.want)
			}
		})
	}
}

// A disposition is the only label a case gets when a mechanism would also
// label it: the producer's close would set clears_on_its_own, and the human's
// needed-human ack must be the one that stands.
func TestAttentionDispositionWinsOverMechanism(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c := attentionCase{name: "x", kind: attention.KindEscalation, key: "esc:hq-9", bead: "hq-9",
		clear: 2 * time.Hour, ack: attention.DispositionNeededHuman, issue: escalationBead("hq-9", "gastown/polecats/furiosa")}
	writeAttentionTown(t, town, []attentionCase{c})

	a := newAttentionTestAdapter(town, beadsOf([]attentionCase{c}))
	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	lr := rep.Lines[0]
	if !sameLabels(lr.Labels, map[string]bool{"needs_human": true}) || lr.Rule != "ack:needed-human" {
		t.Fatalf("labels %v by %q, want only needs_human by ack:needed-human", lr.Labels, lr.Rule)
	}
}

// A bead read that fails for a reason other than not-found fails the report.
func TestAttentionBeadReadFailureFailsTheReport(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c := attentionCase{name: "x", kind: attention.KindEscalation, key: "esc:hq-20", bead: "hq-20", clear: time.Hour,
		issue: escalationBead("hq-20", "overseer")}
	writeAttentionTown(t, town, []attentionCase{c})

	a := newAttentionTestAdapter(town, fakeBeads{err: errors.New("bd timed out")})
	_, err := Build(context.Background(), a, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "reading bead hq-20") {
		t.Fatalf("Build error = %v, want the bead read failure", err)
	}
}

// Only a line whose call answered, for the question set this scorecard reads,
// is an item; anything else is skipped and counted.
func TestAttentionParseSkipsFailedAndForeignLines(t *testing.T) {
	t.Parallel()
	a := newAttentionTestAdapter(t.TempDir(), fakeBeads{})
	line := jevattention.Line{TS: attentionBase, Key: "host-mem:x", Kind: attention.KindHostMem,
		FirstSeen: attentionBase, QuestionVersion: jevattention.QuestionVersion}

	failed := line
	failed.Answers = nil
	foreign := line
	foreign.QuestionVersion = jevattention.QuestionVersion + 1
	foreign.Answers = &jevattention.Answers{}

	for name, l := range map[string]jevattention.Line{"failed call": failed, "other question set": foreign} {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Parse(raw); err == nil {
			t.Errorf("%s: Parse accepted the line", name)
		}
	}
}

// A line at an older question set is still compared: it asked the questions
// the battery keeps, in the same words, and its dropped answer is read when it
// carries one.
func TestAttentionParseReadsAnOlderQuestionSet(t *testing.T) {
	t.Parallel()
	a := newAttentionTestAdapter(t.TempDir(), fakeBeads{})
	line := jevattention.Line{TS: attentionBase, Key: "host-mem:x", Kind: attention.KindHostMem,
		FirstSeen: attentionBase, QuestionVersion: routineQuestionSet, Model: "m",
		Answers: &jevattention.Answers{NeedsHuman: 0.7, ClearsOnItsOwn: 0.2, RoutineActionFixes: ptr(0.1),
			RepeatsOpenItem: 0.4, Urgency: jevattention.Urgency{Score: 2, Confidence: 0.9}}}
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	it, err := a.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !it.Time.Equal(attentionBase) || it.Model != "m" || len(it.Answers) != 5 {
		t.Fatalf("item time %v model %q answers %d, want the line's time, model and five answers", it.Time, it.Model, len(it.Answers))
	}
	if got := it.Answers[jevattention.QNeedsHuman].Noul; got == nil || *got != 0.7 {
		t.Errorf("needs_human noul = %v, want 0.7", got)
	}
	if got := it.Answers[jevattention.QRoutineActionFixes].Noul; got == nil || *got != 0.1 {
		t.Errorf("routine_action_fixes noul = %v, want 0.1", got)
	}
	if got := it.Answers[jevattention.QUrgency].Score; got == nil || *got != 2 {
		t.Errorf("urgency score = %v, want 2", got)
	}
}

// A line logged after routine_action_fixes left the battery has no answer for
// it, so the scorecard leaves the question unanswered rather than scoring 0.
func TestAttentionParseLeavesRoutineUnansweredWhenNotLogged(t *testing.T) {
	t.Parallel()
	a := newAttentionTestAdapter(t.TempDir(), fakeBeads{})
	line := jevattention.Line{TS: attentionBase, Key: "host-mem:x", Kind: attention.KindHostMem,
		FirstSeen: attentionBase, QuestionVersion: jevattention.QuestionVersion, Model: "m",
		Answers: &jevattention.Answers{NeedsHuman: 0.7, ClearsOnItsOwn: 0.2, RepeatsOpenItem: 0.4,
			Urgency: jevattention.Urgency{Score: 2, Confidence: 0.9}}}
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	it, err := a.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, ok := it.Answers[jevattention.QRoutineActionFixes]; ok {
		t.Errorf("routine_action_fixes = %+v, want no answer: the line never logged one", got)
	}
	if len(it.Answers) != 4 {
		t.Errorf("answers = %d, want the four the line logged", len(it.Answers))
	}
}

// A line at the current question set gave no routine_action_fixes answer, so a
// disposition that would label it is not scored: a label with no answer behind
// it is counted as unmatched, and the line is left unlabeled instead.
func TestAttentionLeavesRoutineUnlabeledWithoutItsAnswer(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c := attentionCase{name: "x", kind: attention.KindRejectedTwice, key: "rework:gt-w5", bead: "gt-w5",
		clear: 3 * time.Hour, ack: attention.DispositionFixedByOverseer,
		issue: &beads.Issue{ID: "gt-w5", Status: "closed", CloseReason: "Landed on main"}}
	writeAttentionTown(t, town, []attentionCase{c})

	rep, err := Build(context.Background(), newAttentionTestAdapter(town, beadsOf([]attentionCase{c})), time.Time{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if lr := rep.Lines[0]; lr.Labeled || len(lr.Labels) != 0 {
		t.Fatalf("labeled %v by %q, want unlabeled: the line has no routine_action_fixes answer", lr.Labels, lr.Rule)
	}
}

// The attention scorecard's mechanism labeling, the stale pass and the
// dispatch score read a cleared bead's close reason through one verdict, so
// none of them can call a reason landed while another calls it superseded.
// Three rows diverged before they shared it: "duplicate of gt-a1" was
// superseded only to the stale pass, "landed on main" was landed only to the
// attention scorecard, and a bare "landed" landed only to stale.IsLandedReason.
func TestMechanismLabelingAgreesWithStaleClassification(t *testing.T) {
	t.Parallel()
	// A landing's record names the branch it came from, and the mechanism
	// reads a landing from a polecat branch as the routine route.
	const polecatRecord = "LANDING RECORD\nlanded_commit: c0ffee\nbranch: polecat/furiosa/gt-a1+x9"
	tests := []struct {
		name   string
		reason string
		// want is the verdict all three must reach.
		want string
	}{
		{name: "duplicate of another bead", reason: "duplicate of gt-a1", want: "superseded"},
		{name: "landed on, lower case", reason: "landed on main", want: "landed"},
		{name: "landed on", reason: "Landed on main", want: "landed"},
		{name: "bare landed", reason: "landed", want: "landed"},
		{name: "landed commit note", reason: "Landed on main\nlanded_commit: c0ffee", want: "landed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := &beads.Issue{ID: "gt-a1", Status: "closed", CloseReason: tt.reason, Notes: polecatRecord}
			got := ""
			if lab, ok := mechanismLabeling(attention.KindRejectedTwice, "", issue); ok {
				switch lab.Rule {
				case ruleRejectedLandedRoutine:
					got = "landed"
				case ruleRejectedSuperseded:
					got = "superseded"
				default:
					t.Fatalf("mechanismLabeling named rule %q, want a landed or superseded one", lab.Rule)
				}
			}
			if got != tt.want {
				t.Errorf("mechanismLabeling(%q) = %q, want %q", tt.reason, got, tt.want)
			}
			wantLanded := tt.want == "landed"
			if landed := stale.IsLandedReason(tt.reason); landed != wantLanded {
				t.Errorf("stale.IsLandedReason(%q) = %v, want %v", tt.reason, landed, wantLanded)
			}
			if landed := isLanded(issue); landed != wantLanded {
				t.Errorf("isLanded(%q) = %v, want %v", tt.reason, landed, wantLanded)
			}
		})
	}
}

// The JSON report names the rule for every line, so a label can be traced.
func TestAttentionJSONNamesTheRule(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cases := attentionCases()
	writeAttentionTown(t, town, cases)

	rep, err := Build(context.Background(), newAttentionTestAdapter(town, beadsOf(cases)), time.Time{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"ack:noise", "escalation:closed-by-person", "risk-path:review-fail"} {
		if !strings.Contains(string(out), `"rule":"`+rule+`"`) {
			t.Errorf("JSON report does not name %s", rule)
		}
	}
}

// The text scorecard for the attention battery, one block per question, over
// every case in the test town.
func TestAttentionScorecardGolden(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cases := attentionCases()
	writeAttentionTown(t, town, cases)

	rep, err := Build(context.Background(), newAttentionTestAdapter(town, beadsOf(cases)), time.Time{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var out bytes.Buffer
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "attention_scorecard.golden")
	if *updateAttentionScorecardGolden {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-attention-scorecard-golden):\n%s", golden, out.String())
	}
}

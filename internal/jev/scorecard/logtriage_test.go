package scorecard

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
)

var updateLogtriageGolden = flag.Bool("update-logtriage-golden", false, "rewrite testdata/logtriage.golden")

// logtriageNow is when the fixture is read. A group whose last line is less
// than an hour before it stays unlabeled.
var logtriageNow = time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)

// logtriageFixtureLog is the triage log the golden report reads. Line A is a
// landing group that a critical escalation and an operator event follow within
// the hour. Line B is a dolt group whose only event falls after its window, and
// whose in-window event was cleared. Line C is a polecat group whose only
// follow-up is self-healed. Line D is younger than an hour. Line E is a config
// group that a medium escalation follows. Line F has no answers and line G is
// from another question set, so both are skipped.
var logtriageFixtureLog = []string{
	`{"ts":"2026-10-21T09:45:00Z","key":"landing stuck <id> in flight","count":2,"first":"2026-10-21T09:20:00Z","last":"2026-10-21T09:30:00Z","samples":["2026/10/21 09:20:00 warning: landing gt-abc12 stuck in flight","2026/10/21 09:30:00 warning: landing gt-abc12 stuck in flight"],"answers":{"is_routine_noise":{"noul":0.1},"needs_action":{"noul":0.9},"auto_remediable":{"noul":0.2},"severity":{"score":2,"confidence":0.8},"category":{"choice":"landing","probabilities":{"landing":0.9}}},"question_version":1,"model":"m1"}`,
	`{"ts":"2026-10-21T09:45:00Z","key":"dolt query timeout after <dur>","count":1,"first":"2026-10-21T08:00:00Z","last":"2026-10-21T08:00:00Z","samples":["2026/10/21 08:00:00 error: dolt query timeout after 5s"],"answers":{"is_routine_noise":{"noul":0.3},"needs_action":{"noul":0.2},"auto_remediable":{"noul":0.1},"severity":{"score":1,"confidence":0.6},"category":{"choice":"dolt","probabilities":{"dolt":0.8}}},"question_version":1,"model":"m1"}`,
	`{"ts":"2026-10-21T09:45:00Z","key":"polecat hook empty for gt-pq7","count":1,"first":"2026-10-21T09:00:00Z","last":"2026-10-21T09:00:00Z","samples":["2026/10/21 09:00:00 warning: polecat capable hook empty for gt-pq7"],"answers":{"is_routine_noise":{"noul":0.4},"needs_action":{"noul":0.5},"auto_remediable":{"noul":0.7},"severity":{"score":1,"confidence":0.5},"category":{"choice":"polecat","probabilities":{"polecat":0.7}}},"question_version":1,"model":"m1"}`,
	`{"ts":"2026-10-21T13:45:00Z","key":"gate lint failed <n>","count":1,"first":"2026-10-21T13:30:00Z","last":"2026-10-21T13:30:00Z","samples":["2026/10/21 13:30:00 warning: gate lint failed 3 times"],"answers":{"is_routine_noise":{"noul":0.2},"needs_action":{"noul":0.6},"auto_remediable":{"noul":0.3},"severity":{"score":2,"confidence":0.7},"category":{"choice":"gate","probabilities":{"gate":0.9}}},"question_version":1,"model":"m1"}`,
	`{"ts":"2026-10-21T09:45:00Z","key":"config rig settings missing for <id>","count":1,"first":"2026-10-21T08:30:00Z","last":"2026-10-21T08:30:00Z","samples":["2026/10/21 08:30:00 warning: config rig settings missing for gt-xy9"],"answers":{"is_routine_noise":{"noul":0.1},"needs_action":{"noul":0.8},"auto_remediable":{"noul":0.1},"severity":{"score":2,"confidence":0.9},"category":{"choice":"config","probabilities":{"config":0.95}}},"question_version":1,"model":"m1"}`,
	`{"ts":"2026-10-21T09:45:00Z","key":"panic in worker <n>","count":1,"first":"2026-10-21T09:00:00Z","last":"2026-10-21T09:00:00Z","samples":["2026/10/21 09:00:00 panic in worker"],"answers":null,"question_version":1,"model":"","reason":"ask_failed"}`,
	`{"ts":"2026-10-21T09:45:00Z","key":"old question set <n>","count":1,"first":"2026-10-21T09:00:00Z","last":"2026-10-21T09:00:00Z","samples":["2026/10/21 09:00:00 warning: old question set"],"answers":{"needs_action":{"noul":0.5}},"question_version":0,"model":"m0"}`,
}

// logtriageFixtureEvents are the attention events the town raised, as the
// golden report reads them from events.jsonl.
func logtriageFixtureEvents() []attention.Event {
	return []attention.Event{
		{TS: time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC), Class: attention.KindRejectedTwice, Text: "dolt connection refused", State: attention.EventCleared, Handled: attention.HandledOperatorAction},
		{TS: time.Date(2026, 10, 21, 9, 20, 0, 0, time.UTC), Class: attention.KindRejectedTwice, Text: "polecat capable hook refreshed for gt-pq7", State: attention.EventNew, Handled: attention.HandledSelfHealed},
		{TS: time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC), Class: attention.KindRejectedTwice, Text: "dolt connection refused again", State: attention.EventNew, Handled: attention.HandledOperatorAction},
		{TS: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC), Class: attention.KindRejectedTwice, Text: "landing gt-abc12 stuck in flight", State: attention.EventNew, Handled: attention.HandledOperatorAction},
	}
}

// logtriageFixtureEscalations are the escalation beads the town holds, one
// critical for the landing group and one medium for the config group.
func logtriageFixtureEscalations() []*beads.Issue {
	return []*beads.Issue{
		logtriageFixtureEscalation("gt-esc1", "Landing stuck on gt-abc12", "critical", time.Date(2026, 10, 21, 10, 15, 0, 0, time.UTC)),
		logtriageFixtureEscalation("gt-esc2", "Config: rig settings missing", "medium", time.Date(2026, 10, 21, 9, 10, 0, 0, time.UTC)),
	}
}

func logtriageFixtureEscalation(id, title, severity string, at time.Time) *beads.Issue {
	return &beads.Issue{
		ID:          id,
		Title:       title,
		Status:      "closed",
		CreatedAt:   at.Format(time.RFC3339),
		Description: beads.FormatEscalationDescription(title, &beads.EscalationFields{Severity: severity, Reason: "test", EscalatedAt: at.Format(time.RFC3339)}),
	}
}

// newLogtriageFixtureAdapter is the adapter over the fixture log, events and
// escalations, read at now. It opens no bead database, so the test runs under
// the hermetic harness.
func newLogtriageFixtureAdapter(t *testing.T, now time.Time) *logtriageAdapter {
	t.Helper()
	log := filepath.Join(t.TempDir(), "logtriage.jsonl")
	if err := os.WriteFile(log, []byte(strings.Join(logtriageFixtureLog, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return newLogtriageAdapter(log,
		func() ([]attention.Event, error) { return logtriageFixtureEvents(), nil },
		func() ([]*beads.Issue, error) { return logtriageFixtureEscalations(), nil },
		func() time.Time { return now },
	)
}

// TestLogtriageScorecardPrintsBlockPerQuestion is the logtriage battery's golden
// test. It reads the fixture log with the fixture events and escalations and
// checks the block each question prints.
func TestLogtriageScorecardPrintsBlockPerQuestion(t *testing.T) {
	t.Parallel()
	a := newLogtriageFixtureAdapter(t, logtriageNow)

	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 5 || rep.LinesSkipped != 2 {
		t.Fatalf("items %d skipped %d, want 5 and 2", rep.Items, rep.LinesSkipped)
	}

	var out strings.Builder
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	goldenPath := filepath.Join("testdata", "logtriage.golden")
	if *updateLogtriageGolden {
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-logtriage-golden):\n%s\nwant:\n%s", goldenPath, got, want)
	}
}

// TestLogtriageLabelRules checks the label each group takes, by the rule that
// set it. Each group is parsed from the fixture log in the order it appears.
func TestLogtriageLabelRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		line    int
		now     time.Time
		labeled bool
		labels  map[string]bool
		rule    string
	}{
		{
			name: "a critical escalation and an operator event are a follow-up that pages",
			line: 0, now: logtriageNow, labeled: true,
			labels: map[string]bool{"needs_action": true, "is_routine_noise": false, "auto_remediable": false, "severity": true},
			rule:   ruleLogtriageFollowUp + "+" + ruleLogtriageNotHealed,
		},
		{
			name: "an event past the window and a cleared event are no follow-up",
			line: 1, now: logtriageNow, labeled: true,
			labels: map[string]bool{"needs_action": false, "is_routine_noise": true, "auto_remediable": false},
			rule:   ruleLogtriageQuiet + "+" + ruleLogtriageNotHealed,
		},
		{
			name: "a self-healed event sets auto_remediable and no action",
			line: 2, now: logtriageNow, labeled: true,
			labels: map[string]bool{"needs_action": false, "is_routine_noise": true, "auto_remediable": true},
			rule:   ruleLogtriageQuiet + "+" + ruleLogtriageSelfHealed,
		},
		{
			name: "a group younger than an hour stays unlabeled",
			line: 3, now: logtriageNow, labeled: false,
		},
		{
			name: "a medium escalation is a follow-up that is a review",
			line: 4, now: logtriageNow, labeled: true,
			labels: map[string]bool{"needs_action": true, "is_routine_noise": false, "auto_remediable": false, "severity": false},
			rule:   ruleLogtriageFollowUp + "+" + ruleLogtriageNotHealed,
		},
		{
			name: "a group exactly an hour old is labeled",
			line: 1, now: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), labeled: true,
			labels: map[string]bool{"needs_action": false, "is_routine_noise": true, "auto_remediable": false},
			rule:   ruleLogtriageQuiet + "+" + ruleLogtriageNotHealed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newLogtriageFixtureAdapter(t, tc.now)
			items := parseLogtriageFixture(t, a)
			lab, err := a.Label(context.Background(), items[tc.line])
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled != tc.labeled {
				t.Fatalf("labeled %v, want %v", lab.Labeled, tc.labeled)
			}
			if !tc.labeled {
				return
			}
			got := map[string]bool{}
			for id, v := range lab.Labels {
				got[string(id)] = v
			}
			if !reflect.DeepEqual(got, tc.labels) {
				t.Errorf("labels %v, want %v", got, tc.labels)
			}
			if lab.Rule != tc.rule {
				t.Errorf("rule %q, want %q", lab.Rule, tc.rule)
			}
		})
	}
}

// TestLogtriageLabelsAFixtureOfGroups labels the fixture's groups against one
// set of events and escalations, and compares every label and rule with the
// stored expectation. The set holds a zero-timestamp event, a readable event
// inside a window, one after it, and an escalation whose raised time does not
// parse.
func TestLogtriageLabelsAFixtureOfGroups(t *testing.T) {
	t.Parallel()
	events := []attention.Event{
		{Text: "landing gt-abc12 stuck in flight", State: attention.EventNew, Handled: attention.HandledOperatorAction},
		{TS: time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC), Text: "dolt connection refused", State: attention.EventNew, Handled: attention.HandledOperatorAction},
		{TS: time.Date(2026, 10, 21, 10, 30, 0, 0, time.UTC), Text: "polecat capable hook empty for gt-pq7", State: attention.EventNew, Handled: attention.HandledOperatorAction},
	}
	escalations := []*beads.Issue{
		{ID: "gt-esc-bad", Title: "Config rig settings missing", CreatedAt: "not a time"},
	}
	// The stored result, one line per fixture group.
	want := []struct {
		name    string
		labeled bool
		labels  map[string]bool
		rule    string
	}{
		{
			name: "the zero-timestamp event names the landing group, so it could fall in the window and the group is unlabeled",
		},
		{
			name:    "the event inside the dolt group's window is a follow-up",
			labeled: true,
			labels:  map[string]bool{"needs_action": true, "is_routine_noise": false, "auto_remediable": false},
			rule:    ruleLogtriageFollowUp + "+" + ruleLogtriageNotHealed,
		},
		{
			name:    "the event after the polecat group's window is no follow-up",
			labeled: true,
			labels:  map[string]bool{"needs_action": false, "is_routine_noise": true, "auto_remediable": false},
			rule:    ruleLogtriageQuiet + "+" + ruleLogtriageNotHealed,
		},
		{
			name: "the gate group is younger than an hour and unlabeled",
		},
		{
			name: "the escalation whose raised time does not parse names the config group, so the group is unlabeled",
		},
	}
	a := logtriageAdapterWith(logtriageNow, events, escalations)
	items := parseLogtriageFixture(t, a)
	if len(items) != len(want) {
		t.Fatalf("fixture has %d groups, want %d", len(items), len(want))
	}
	for i, w := range want {
		w := w
		item := items[i]
		t.Run(w.name, func(t *testing.T) {
			lab, err := a.Label(context.Background(), item)
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled != w.labeled {
				t.Fatalf("labeled %v, want %v (rule %q)", lab.Labeled, w.labeled, lab.Rule)
			}
			if !w.labeled {
				return
			}
			got := map[string]bool{}
			for id, v := range lab.Labels {
				got[string(id)] = v
			}
			if !reflect.DeepEqual(got, w.labels) {
				t.Errorf("labels %v, want %v", got, w.labels)
			}
			if lab.Rule != w.rule {
				t.Errorf("rule %q, want %q", lab.Rule, w.rule)
			}
		})
	}
}

// TestLogtriageSkipsTheMatchForAnEventOutsideTheWindow shows the window check
// runs before the subject pattern: an event after the window whose text names
// the group never reaches the pattern, while an in-window event that does not
// match does reach it. The dolt fixture group has one subject word, "dolt".
func TestLogtriageSkipsTheMatchForAnEventOutsideTheWindow(t *testing.T) {
	t.Parallel()
	events := []attention.Event{
		{TS: time.Date(2026, 10, 21, 10, 30, 0, 0, time.UTC), Text: "dolt connection refused", State: attention.EventNew, Handled: attention.HandledOperatorAction},
		{TS: time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC), Text: "unrelated notice", State: attention.EventNew, Handled: attention.HandledOperatorAction},
	}
	a := logtriageAdapterWith(logtriageNow, events, nil)
	matches := 0
	a.matchWord = func(re *regexp.Regexp, text string) bool {
		matches++
		return re.MatchString(text)
	}
	if _, err := a.Label(context.Background(), parseLogtriageLine(t, a, logtriageFixtureLog[1])); err != nil {
		t.Fatal(err)
	}
	if matches != 1 {
		t.Errorf("the pattern ran %d times, want once, for the in-window event only", matches)
	}
}

// TestLogtriageSkipsAGroupWithoutAnswersOrFromAnotherQuestionSet checks that a
// group whose call failed, or one logged under another question set, is skipped.
func TestLogtriageSkipsAGroupWithoutAnswersOrFromAnotherQuestionSet(t *testing.T) {
	t.Parallel()
	a := newLogtriageAdapter("unused", nil, nil, time.Now)
	if _, err := a.Parse([]byte(logtriageFixtureLog[5])); err == nil {
		t.Error("a group with no answers parsed, want an error")
	}
	if _, err := a.Parse([]byte(logtriageFixtureLog[6])); err == nil {
		t.Error("a group from question set 0 parsed, want an error")
	}
}

// parseLogtriageFixture parses the fixture lines the adapter reads, in order,
// and returns the item for each line that parses.
func parseLogtriageFixture(t *testing.T, a *logtriageAdapter) []Item {
	t.Helper()
	var items []Item
	for _, raw := range logtriageFixtureLog[:5] {
		items = append(items, parseLogtriageLine(t, a, raw))
	}
	return items
}

// parseLogtriageLine parses one raw triage line and returns its item.
func parseLogtriageLine(t *testing.T, a *logtriageAdapter, raw string) Item {
	t.Helper()
	it, err := a.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// logtriageAdapterWith is the adapter over the given events and escalations,
// read at now. Label reads those, never the log file, so the log path is unused.
func logtriageAdapterWith(now time.Time, events []attention.Event, escalations []*beads.Issue) *logtriageAdapter {
	return newLogtriageAdapter("unused",
		func() ([]attention.Event, error) { return events, nil },
		func() ([]*beads.Issue, error) { return escalations, nil },
		func() time.Time { return now },
	)
}

// TestLogtriageSubjectMatchesWholeWordsOnly checks that the category word and a
// bead id match a text only as a whole token, not inside a longer word or id.
func TestLogtriageSubjectMatchesWholeWordsOnly(t *testing.T) {
	t.Parallel()
	subject := newLogtriageSubject(logtriage.Line{
		Answers: map[string]logtriage.Answer{logtriage.QCategory: {Choice: "dolt"}},
		Samples: []string{"2026/10/21 08:00:00 error: dolt query for gt-abc12 timed out"},
	}, nil)
	cases := []struct {
		text string
		want bool
	}{
		{"Dolt: connection refused", true},
		{"dolt query timeout", true},
		{"landing gt-abc12 stuck", true},
		{"landing gt-abc12. stuck", true},
		{"doltish query timeout", false},
		{"landing gt-abc1 stuck", false},
		{"landing gt-abc12x stuck", false},
		{"landing gt-abc12-x stuck", false},
	}
	for _, tc := range cases {
		if got := subject.matches(tc.text); got != tc.want {
			t.Errorf("matches(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// TestLogtriageUnreadableDataLeavesTheGroupUnlabeled checks that a matching
// follow-up whose time cannot be read does not read as no follow-up, while a
// readable follow-up in the hour still sets the label.
func TestLogtriageUnreadableDataLeavesTheGroupUnlabeled(t *testing.T) {
	t.Parallel()
	unreadableEsc := &beads.Issue{ID: "gt-esc3", Title: "Dolt query timeout after 5s", CreatedAt: "not a time"}
	unrelatedUnreadableEsc := &beads.Issue{ID: "gt-esc4", Title: "Landing stuck", CreatedAt: "not a time"}
	unreadableEvent := attention.Event{Text: "dolt is down", State: attention.EventNew, Handled: attention.HandledOperatorAction}
	readableEvent := attention.Event{TS: time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC), Text: "dolt connection refused", State: attention.EventNew, Handled: attention.HandledOperatorAction}
	cases := []struct {
		name        string
		events      []attention.Event
		escalations []*beads.Issue
		labeled     bool
		needsAction bool
	}{
		{name: "an escalation whose time is unreadable is unlabeled", escalations: []*beads.Issue{unreadableEsc}},
		{name: "an event whose time is unreadable is unlabeled", events: []attention.Event{unreadableEvent}},
		{name: "an unreadable follow-up beside a readable one is a follow-up", events: []attention.Event{readableEvent, unreadableEvent}, labeled: true, needsAction: true},
		{name: "an unreadable escalation that shares no word is no follow-up", escalations: []*beads.Issue{unrelatedUnreadableEsc}, labeled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := logtriageAdapterWith(logtriageNow, tc.events, tc.escalations)
			item := parseLogtriageLine(t, a, logtriageFixtureLog[1])
			lab, err := a.Label(context.Background(), item)
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled != tc.labeled {
				t.Fatalf("labeled %v, want %v", lab.Labeled, tc.labeled)
			}
			if tc.labeled && lab.Labels[QuestionID(logtriage.QNeedsAction)] != tc.needsAction {
				t.Errorf("needs_action %v, want %v", lab.Labels[QuestionID(logtriage.QNeedsAction)], tc.needsAction)
			}
			// is_routine_noise is true only for a quiet group whose data all
			// read: an unreadable follow-up beside a readable one is a
			// follow-up (false), and an unreadable one alone leaves it unset.
			if tc.labeled {
				if v, ok := lab.Labels[QuestionID(logtriage.QRoutineNoise)]; !ok || v != !tc.needsAction {
					t.Errorf("is_routine_noise %v (set %v), want %v", v, ok, !tc.needsAction)
				}
			}
			if !tc.labeled {
				for _, q := range []string{logtriage.QRoutineNoise, logtriage.QAutoRemediate} {
					if _, ok := lab.Labels[QuestionID(q)]; ok {
						t.Errorf("%s is labeled from data the adapter could not read", q)
					}
				}
			}
			// An unresolved unreadable follow-up could be self-healed, so
			// auto_remediable gets no false beside it.
			if tc.labeled && tc.needsAction {
				if v, ok := lab.Labels[QuestionID(logtriage.QAutoRemediate)]; ok {
					t.Errorf("auto_remediable labeled %v beside an unreadable follow-up", v)
				}
			}
		})
	}
}

// logtriageNoSubjectLine is a group with no category and no bead id.
const logtriageNoSubjectLine = `{"ts":"2026-10-21T09:45:00Z","key":"something <n>","count":1,"first":"2026-10-21T08:00:00Z","last":"2026-10-21T08:00:00Z","samples":["2026/10/21 08:00:00 something happened"],"answers":{"needs_action":{"noul":0.5}},"question_version":1,"model":"m1"}`

// TestLogtriageGroupWithNoSubjectStaysUnlabeled checks that a group with no
// category and no bead id, which nothing could be a follow-up to, is unlabeled
// even when an event in its hour is raised.
func TestLogtriageGroupWithNoSubjectStaysUnlabeled(t *testing.T) {
	t.Parallel()
	events := []attention.Event{{TS: time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC), Text: "something happened", State: attention.EventNew, Handled: attention.HandledOperatorAction}}
	a := logtriageAdapterWith(logtriageNow, events, nil)
	lab, err := a.Label(context.Background(), parseLogtriageLine(t, a, logtriageNoSubjectLine))
	if err != nil {
		t.Fatal(err)
	}
	if lab.Labeled {
		t.Errorf("labeled with rule %q, want unlabeled", lab.Rule)
	}
}

// TestLogtriageSeverityPagesOnlyCriticalAndHigh checks the severity label an
// escalation sets: true for critical and high, false for the rest.
func TestLogtriageSeverityPagesOnlyCriticalAndHigh(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		severity string
		want     bool
	}{
		{"critical", true},
		{"high", true},
		{"HIGH", true},
		{"medium", false},
		{"low", false},
		{"", false},
	} {
		t.Run(tc.severity, func(t *testing.T) {
			t.Parallel()
			esc := logtriageFixtureEscalation("gt-esc5", "Dolt query timeout after 5s", tc.severity, time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC))
			a := logtriageAdapterWith(logtriageNow, nil, []*beads.Issue{esc})
			lab, err := a.Label(context.Background(), parseLogtriageLine(t, a, logtriageFixtureLog[1]))
			if err != nil {
				t.Fatal(err)
			}
			if got := lab.Labels[QuestionID(logtriage.QSeverity)]; got != tc.want {
				t.Errorf("severity %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLogtriageSelfHealedBesideAFollowUp checks that a self-healed event and a
// readable follow-up in the same hour both label the group.
func TestLogtriageSelfHealedBesideAFollowUp(t *testing.T) {
	t.Parallel()
	events := []attention.Event{
		{TS: time.Date(2026, 10, 21, 9, 20, 0, 0, time.UTC), Text: "polecat capable hook refreshed for gt-pq7", State: attention.EventNew, Handled: attention.HandledSelfHealed},
		{TS: time.Date(2026, 10, 21, 9, 40, 0, 0, time.UTC), Text: "polecat capable hook empty for gt-pq7", State: attention.EventNew, Handled: attention.HandledOperatorAction},
	}
	a := logtriageAdapterWith(logtriageNow, events, nil)
	lab, err := a.Label(context.Background(), parseLogtriageLine(t, a, logtriageFixtureLog[2]))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"needs_action": true, "is_routine_noise": false, "auto_remediable": true}
	got := map[string]bool{}
	for id, v := range lab.Labels {
		got[string(id)] = v
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels %v, want %v", got, want)
	}
	if wantRule := ruleLogtriageFollowUp + "+" + ruleLogtriageSelfHealed; lab.Rule != wantRule {
		t.Errorf("rule %q, want %q", lab.Rule, wantRule)
	}
}

// TestLogtriageReadErrorsFailTheLabel checks that a failure reading the events
// or the escalations fails the label rather than reading as no follow-up.
func TestLogtriageReadErrorsFailTheLabel(t *testing.T) {
	t.Parallel()
	readErr := errors.New("unreadable")
	failEvents := func() ([]attention.Event, error) { return nil, readErr }
	failEscalations := func() ([]*beads.Issue, error) { return nil, readErr }
	noEvents := func() ([]attention.Event, error) { return nil, nil }
	noEscalations := func() ([]*beads.Issue, error) { return nil, nil }
	cases := []struct {
		name        string
		events      func() ([]attention.Event, error)
		escalations func() ([]*beads.Issue, error)
	}{
		{name: "the events fail to read", events: failEvents, escalations: noEscalations},
		{name: "the escalations fail to read", events: noEvents, escalations: failEscalations},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newLogtriageAdapter("unused", tc.events, tc.escalations, func() time.Time { return logtriageNow })
			item := parseLogtriageLine(t, a, logtriageFixtureLog[1])
			if _, err := a.Label(context.Background(), item); err == nil {
				t.Error("Label returned no error, want the read failure")
			}
		})
	}
}

// TestLogtriageLabelRejectsAnUnparsedItem checks that Label refuses an item
// the adapter never parsed, since it has no group to judge.
func TestLogtriageLabelRejectsAnUnparsedItem(t *testing.T) {
	t.Parallel()
	a := logtriageAdapterWith(logtriageNow, nil, nil)
	if _, err := a.Label(context.Background(), Item{ID: "never-parsed"}); err == nil {
		t.Error("Label of an unparsed item returned no error")
	}
}

// TestLogtriageParseRejectsMalformedLines checks that a line that is not JSON,
// or has no key or no ts, is rejected.
func TestLogtriageParseRejectsMalformedLines(t *testing.T) {
	t.Parallel()
	a := newLogtriageAdapter("unused", nil, nil, time.Now)
	for _, raw := range []string{
		`not json`,
		`{"ts":"2026-10-21T09:45:00Z","answers":{},"question_version":1}`,
		`{"key":"dolt query timeout after <dur>","answers":{},"question_version":1}`,
	} {
		if _, err := a.Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%q) returned no error", raw)
		}
	}
}

// TestLogtriageAutoRemediable checks both outcomes of auto_remediable for a
// group that has a subject, and that a group with none gets no label.
func TestLogtriageAutoRemediable(t *testing.T) {
	t.Parallel()
	healed := attention.Event{TS: time.Date(2026, 10, 21, 9, 20, 0, 0, time.UTC), Text: "polecat capable hook refreshed for gt-pq7", State: attention.EventNew, Handled: attention.HandledSelfHealed}
	operator := attention.Event{TS: time.Date(2026, 10, 21, 9, 20, 0, 0, time.UTC), Text: "polecat capable hook empty for gt-pq7", State: attention.EventNew, Handled: attention.HandledOperatorAction}
	cases := []struct {
		name    string
		line    int
		events  []attention.Event
		labeled bool
		want    bool
	}{
		{name: "a self-healed follow-up is true", line: 2, events: []attention.Event{healed}, labeled: true, want: true},
		{name: "an operator follow-up and no self-healed one is false", line: 2, events: []attention.Event{operator}, labeled: true},
		{name: "no follow-up at all is false", line: 2, labeled: true},
		{name: "a group with no subject is unlabeled", line: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := logtriageAdapterWith(logtriageNow, tc.events, nil)
			var item Item
			if tc.line < 0 {
				item = parseLogtriageLine(t, a, logtriageNoSubjectLine)
			} else {
				item = parseLogtriageLine(t, a, logtriageFixtureLog[tc.line])
			}
			lab, err := a.Label(context.Background(), item)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := lab.Labels[QuestionID(logtriage.QAutoRemediate)]
			if ok != tc.labeled || got != tc.want {
				t.Errorf("auto_remediable labeled %v value %v, want labeled %v value %v", ok, got, tc.labeled, tc.want)
			}
		})
	}
}

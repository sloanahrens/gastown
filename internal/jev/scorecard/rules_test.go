package scorecard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/rules"
)

var rulesDoneAt = time.Date(2026, 10, 21, 17, 0, 0, 0, time.UTC)

// rulesRaw is one logged rules line for gt-x, submitted at rulesDoneAt, with a
// positive broke answer and a main_push choice.
func rulesRaw(t *testing.T, bead string) []byte {
	t.Helper()
	noul := 0.9
	raw, err := json.Marshal(rules.Line{
		TS:              rulesDoneAt.Add(time.Minute),
		Rig:             "gastown",
		Bead:            bead,
		Polecat:         "bullet",
		DoneAt:          rulesDoneAt,
		Model:           "jev-test",
		Answers:         map[string]rules.Answer{rules.QBrokeStandingRule: {Noul: &noul}, rules.QRuleKind: {Choice: rules.KindMainPush, Probabilities: map[string]float64{rules.KindMainPush: 1}}},
		QuestionVersion: rules.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// rulesAdapterAt is the rules adapter for town, its clock at now, and the
// comments a fake bead store returns.
func rulesAdapterAt(town string, now time.Time, comments map[string][]beads.Comment) *rulesAdapter {
	return &rulesAdapter{
		town: town,
		comments: func(_, bead string) ([]beads.Comment, error) {
			return comments[bead], nil
		},
		lines: map[string]rules.Line{},
		now:   func() time.Time { return now },
	}
}

// labelRules parses one line and labels it, the way the report does.
func labelRules(t *testing.T, a *rulesAdapter, raw []byte) Labeling {
	t.Helper()
	it, err := a.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	lab, err := a.Label(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	return lab
}

// TestRulesParseSkipsUnaskedAndOldLines checks a session with no answers, and a
// line from another question set, are skipped rather than compared.
func TestRulesParseSkipsUnaskedAndOldLines(t *testing.T) {
	t.Parallel()
	a := rulesAdapterAt(t.TempDir(), rulesDoneAt, nil)

	unasked, _ := json.Marshal(rules.Line{TS: rulesDoneAt, Rig: "gastown", Bead: "gt-x", DoneAt: rulesDoneAt,
		QuestionVersion: rules.Version, Reason: rules.ReasonNoTranscript})
	if _, err := a.Parse(unasked); err == nil || !strings.Contains(err.Error(), rules.ReasonNoTranscript) {
		t.Errorf("unasked line: err = %v, want it skipped with its reason", err)
	}

	old := strings.Replace(string(rulesRaw(t, "gt-x")), `"question_version":1`, `"question_version":0`, 1)
	if _, err := a.Parse([]byte(old)); err == nil {
		t.Error("line from another question set was not skipped")
	}
}

// TestRulesLabelsPositiveFromRevertGuard checks a revert-guard refusal of the
// bead inside the window labels the session broke a rule, and names the source.
func TestRulesLabelsPositiveFromRevertGuard(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := attention.AppendRefusal(town, attention.Refusal{
		TS: rulesDoneAt.Add(2 * time.Minute), Bead: "gt-x", Rig: "gastown", Worker: "bullet",
		Head: "abc1234", Kind: attention.KindRevertGuard, Summary: "reverts a merged commit",
	}); err != nil {
		t.Fatal(err)
	}
	a := rulesAdapterAt(town, rulesDoneAt.Add(25*time.Minute), nil)

	lab := labelRules(t, a, rulesRaw(t, "gt-x"))
	if !lab.Labeled || !lab.Labels[qBrokeStandingRule] || lab.Rule != ruleRulesRevertGuard {
		t.Errorf("labeling = %+v, want broke true from %s", lab, ruleRulesRevertGuard)
	}
	if _, ok := lab.Labels[qRuleKind]; ok {
		t.Error("rule_kind was labeled; no guard record names a kind")
	}
}

// TestRulesLabelsNegativeAfterWindow checks a session with no guard record
// inside the window is labeled not broken once the window has passed.
func TestRulesLabelsNegativeAfterWindow(t *testing.T) {
	t.Parallel()
	a := rulesAdapterAt(t.TempDir(), rulesDoneAt.Add(25*time.Minute), nil)

	lab := labelRules(t, a, rulesRaw(t, "gt-x"))
	if !lab.Labeled || lab.Labels[qBrokeStandingRule] || lab.Rule != ruleRulesNoRecord {
		t.Errorf("labeling = %+v, want broke false from %s", lab, ruleRulesNoRecord)
	}
}

// TestRefusalSourcedLineIsNotLabeledByItsOwnRefusal checks a line made from a
// revert-guard refusal is not labeled broke by that refusal. The refusal's time
// is the line's submission, so it sits on the window's start and must not count.
func TestRefusalSourcedLineIsNotLabeledByItsOwnRefusal(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := attention.AppendRefusal(town, attention.Refusal{
		TS: rulesDoneAt, Bead: "gt-x", Rig: "gastown", Worker: "bullet",
		Head: "abc1234", Kind: attention.KindRevertGuard, Summary: "reverts a merged commit",
	}); err != nil {
		t.Fatal(err)
	}
	a := rulesAdapterAt(town, rulesDoneAt.Add(25*time.Minute), nil)

	lab := labelRules(t, a, rulesRaw(t, "gt-x"))
	if !lab.Labeled || lab.Labels[qBrokeStandingRule] || lab.Rule != ruleRulesNoRecord {
		t.Errorf("labeling = %+v, want broke false from %s: the refusal that made the line labeled it", lab, ruleRulesNoRecord)
	}
}

// TestRulesStaysUnlabeledInsideWindow checks a session younger than the window
// is not labeled, since a record may still be coming.
func TestRulesStaysUnlabeledInsideWindow(t *testing.T) {
	t.Parallel()
	a := rulesAdapterAt(t.TempDir(), rulesDoneAt.Add(5*time.Minute), nil)

	if lab := labelRules(t, a, rulesRaw(t, "gt-x")); lab.Labeled {
		t.Errorf("labeling = %+v inside the window, want unlabeled", lab)
	}
}

// TestRulesLabelsFromReworkStandDown checks the close-skipped comment of a
// rework stand-down labels the session, and a refusal outside the window does
// not.
func TestRulesLabelsFromReworkStandDown(t *testing.T) {
	t.Parallel()
	comments := map[string][]beads.Comment{
		"gt-x": {{
			Author:    "gastown/polecats/bullet",
			Text:      "DONE_CLOSE_SKIPPED: head unchanged since the rejection — nothing was resubmitted",
			CreatedAt: rulesDoneAt.Add(4 * time.Minute).Format(time.RFC3339Nano),
		}},
	}
	a := rulesAdapterAt(t.TempDir(), rulesDoneAt.Add(25*time.Minute), comments)
	lab := labelRules(t, a, rulesRaw(t, "gt-x"))
	if !lab.Labeled || !lab.Labels[qBrokeStandingRule] || lab.Rule != ruleRulesStandDown {
		t.Errorf("labeling = %+v, want broke true from %s", lab, ruleRulesStandDown)
	}

	late := map[string][]beads.Comment{
		"gt-x": {{
			Text:      "DONE_CLOSE_SKIPPED: head unchanged since the rejection",
			CreatedAt: rulesDoneAt.Add(40 * time.Minute).Format(time.RFC3339Nano),
		}},
	}
	b := rulesAdapterAt(t.TempDir(), rulesDoneAt.Add(45*time.Minute), late)
	if lab := labelRules(t, b, rulesRaw(t, "gt-x")); lab.Labels[qBrokeStandingRule] {
		t.Errorf("a stand-down after the window labeled the session broke: %+v", lab)
	}
}

// TestLabelIgnoresRefusalOfAnotherWorker checks a revert-guard refusal of the
// same bead inside the window, by a different polecat, does not label this
// session. The bead was re-slung after a refusal of an earlier submission, so
// the refusal belongs to the worker that made it, not to the line's session.
func TestLabelIgnoresRefusalOfAnotherWorker(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := attention.AppendRefusal(town, attention.Refusal{
		TS: rulesDoneAt.Add(2 * time.Minute), Bead: "gt-x", Rig: "gastown", Worker: "capable",
		Head: "abc1234", Kind: attention.KindRevertGuard, Summary: "reverts a merged commit",
	}); err != nil {
		t.Fatal(err)
	}
	a := rulesAdapterAt(town, rulesDoneAt.Add(25*time.Minute), nil)

	lab := labelRules(t, a, rulesRaw(t, "gt-x"))
	if !lab.Labeled || lab.Labels[qBrokeStandingRule] || lab.Rule != ruleRulesNoRecord {
		t.Errorf("labeling = %+v, want broke false from %s: another worker's refusal labeled this session", lab, ruleRulesNoRecord)
	}
}

// TestLabelIgnoresStandDownOfAnotherWorker checks a rework stand-down comment
// written under another polecat's name does not label this session.
func TestLabelIgnoresStandDownOfAnotherWorker(t *testing.T) {
	t.Parallel()
	comments := map[string][]beads.Comment{
		"gt-x": {{
			Author:    "gastown/polecats/capable",
			Text:      "DONE_CLOSE_SKIPPED: head unchanged since the rejection",
			CreatedAt: rulesDoneAt.Add(4 * time.Minute).Format(time.RFC3339Nano),
		}},
	}
	a := rulesAdapterAt(t.TempDir(), rulesDoneAt.Add(25*time.Minute), comments)

	lab := labelRules(t, a, rulesRaw(t, "gt-x"))
	if !lab.Labeled || lab.Labels[qBrokeStandingRule] || lab.Rule != ruleRulesNoRecord {
		t.Errorf("labeling = %+v, want broke false from %s: another worker's stand-down labeled this session", lab, ruleRulesNoRecord)
	}
}

// TestRulesUnattributedRecordStaysUnlabeled checks a refusal with no worker, or
// a stand-down comment with no author, inside the window leaves the session
// unlabeled: the record may be this session's, and it cannot be told apart.
func TestRulesUnattributedRecordStaysUnlabeled(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := attention.AppendRefusal(town, attention.Refusal{
		TS: rulesDoneAt.Add(2 * time.Minute), Bead: "gt-x", Rig: "gastown",
		Head: "abc1234", Kind: attention.KindRevertGuard, Summary: "reverts a merged commit",
	}); err != nil {
		t.Fatal(err)
	}
	comments := map[string][]beads.Comment{
		"gt-x": {{
			Text:      "DONE_CLOSE_SKIPPED: head unchanged since the rejection",
			CreatedAt: rulesDoneAt.Add(4 * time.Minute).Format(time.RFC3339Nano),
		}},
	}
	a := rulesAdapterAt(town, rulesDoneAt.Add(25*time.Minute), comments)

	if lab := labelRules(t, a, rulesRaw(t, "gt-x")); lab.Labeled {
		t.Errorf("labeling = %+v for unattributed records, want unlabeled", lab)
	}
}

// TestRulesContextNamesSourcesAndNotYet checks the report says which guard
// sources were read, and that rule_kind has no record and stays not-yet.
func TestRulesContextNamesSourcesAndNotYet(t *testing.T) {
	t.Parallel()
	a := rulesAdapterAt(t.TempDir(), rulesDoneAt.Add(25*time.Minute), nil)
	labelRules(t, a, rulesRaw(t, "gt-x"))

	ctx := strings.Join(a.Context(), "\n")
	for _, want := range []string{"revert-guard", "rework stand-down", "rule_kind not-yet", "BLOCKED mail is not read"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context is missing %q:\n%s", want, ctx)
		}
	}
}

// TestRulesIsRegistered checks the battery is listed and opens under its name.
func TestRulesIsRegistered(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	a, err := Open(RulesBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	if a.Battery() != RulesBattery || a.LogPath() != rules.LogPath(town) {
		t.Errorf("battery %q logs to %q; want %q to %q", a.Battery(), a.LogPath(), RulesBattery, rules.LogPath(town))
	}
	found := false
	for _, name := range Registered() {
		if name == RulesBattery {
			found = true
		}
	}
	if !found {
		t.Errorf("Registered() = %v, want %q listed", Registered(), RulesBattery)
	}
}

// TestRulesUnknownBeadStaysUnlabeled checks a bead the store does not hold is
// left unlabeled, not labeled not broken: its guard records cannot be read.
func TestRulesUnknownBeadStaysUnlabeled(t *testing.T) {
	t.Parallel()
	a := rulesAdapterAt(t.TempDir(), rulesDoneAt.Add(25*time.Minute), nil)
	a.comments = func(string, string) ([]beads.Comment, error) { return nil, beads.ErrNotFound }

	if lab := labelRules(t, a, rulesRaw(t, "gt-x")); lab.Labeled {
		t.Errorf("labeling = %+v for a bead the store does not hold, want unlabeled", lab)
	}
}

// TestRulesUnreadableStandDownStaysUnlabeled checks a stand-down comment whose
// time cannot be read leaves the session unlabeled: it may be inside the window.
func TestRulesUnreadableStandDownStaysUnlabeled(t *testing.T) {
	t.Parallel()
	comments := map[string][]beads.Comment{
		"gt-x": {{
			Text:      "DONE_CLOSE_SKIPPED: head unchanged since the rejection",
			CreatedAt: "not a time",
		}},
	}
	a := rulesAdapterAt(t.TempDir(), rulesDoneAt.Add(25*time.Minute), comments)

	if lab := labelRules(t, a, rulesRaw(t, "gt-x")); lab.Labeled {
		t.Errorf("labeling = %+v with an unreadable stand-down time, want unlabeled", lab)
	}
}

// TestRulesMissingBeadKeepsARevertGuardLabel checks a bead the store does not
// hold still takes a revert-guard label: the refusal ledger names it, so the
// session broke a rule whatever its comments say.
func TestRulesMissingBeadKeepsARevertGuardLabel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := attention.AppendRefusal(town, attention.Refusal{
		TS: rulesDoneAt.Add(2 * time.Minute), Bead: "gt-x", Rig: "gastown", Worker: "bullet",
		Head: "abc1234", Kind: attention.KindRevertGuard, Summary: "reverts a merged commit",
	}); err != nil {
		t.Fatal(err)
	}
	a := rulesAdapterAt(town, rulesDoneAt.Add(25*time.Minute), nil)
	a.comments = func(string, string) ([]beads.Comment, error) { return nil, beads.ErrNotFound }

	lab := labelRules(t, a, rulesRaw(t, "gt-x"))
	if !lab.Labeled || !lab.Labels[qBrokeStandingRule] || lab.Rule != ruleRulesRevertGuard {
		t.Errorf("labeling = %+v for a missing bead with a refusal, want broke true from %s", lab, ruleRulesRevertGuard)
	}
}

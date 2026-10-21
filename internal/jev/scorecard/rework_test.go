package scorecard

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/rework"
	"github.com/steveyegge/gastown/internal/land"
)

// The refusal of gastown's bead gt-abc at a fixed instant.
var refusedAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// refusedHead is the submission the fixture refusal judged.
const refusedHead = "abc123def456"

// reworkLine is one refusal line as the battery writes it.
func reworkLine(t *testing.T, bead, head string) []byte {
	t.Helper()
	reworked := 0.8
	raw, err := json.Marshal(rework.Line{
		TS:      refusedAt.Add(20 * time.Minute),
		Refused: refusedAt,
		Rig:     "gastown",
		Bead:    bead,
		Attempt: 1,
		Head:    head,
		Verdict: land.VerdictRequestChanges,
		Score:   0.6,
		Answers: map[string]rework.Answer{
			rework.QReworkedToLand: {Noul: &reworked},
			rework.QCause:          {Choice: "tests_missing"},
		},
		Model:           "jev-1.13.0",
		QuestionVersion: rework.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// landedWith writes one landing record for bead at landed with head onto the
// rig's landings file under townRoot.
func landedWith(t *testing.T, townRoot, bead, head string, landed time.Time) {
	t.Helper()
	file, err := land.RigLandingsFile(townRoot, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Append(land.LandingRecord{BeadID: bead, Rig: "gastown", Target: "main",
		Head: head, GateResult: "pass", OMVerdict: land.VerdictApprove, LandedAt: landed}); err != nil {
		t.Fatal(err)
	}
}

// labelRework parses one refusal line through the adapter and labels it.
func labelRework(t *testing.T, townRoot string, raw []byte) Labeling {
	t.Helper()
	a := newReworkAdapter(rework.LogPath(townRoot), townRoot)
	it, err := a.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := a.Label(context.Background(), it)
	if err != nil {
		t.Fatalf("Label: %v", err)
	}
	return got
}

// The refused head landing later, with no rework commit, labels the refusal
// negative.
func TestReworkLabelsSameHeadLandedNegative(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	landedWith(t, town, "gt-abc", refusedHead, refusedAt.Add(2*time.Hour))

	got := labelRework(t, town, reworkLine(t, "gt-abc", refusedHead))

	if !got.Labeled || got.Labels[qReworkedToLand] != false || got.Rule != ruleReworkSameHead {
		t.Errorf("Labeling = %+v, want reworked_to_land false by %s", got, ruleReworkSameHead)
	}
}

// A later submission landing labels the refusal positive: the bead landed only
// after a rework commit.
func TestReworkLabelsALaterHeadPositive(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	landedWith(t, town, "gt-abc", "999999aaaaaa", refusedAt.Add(3*time.Hour))

	got := labelRework(t, town, reworkLine(t, "gt-abc", refusedHead))

	if !got.Labeled || got.Labels[qReworkedToLand] != true || got.Rule != ruleReworkNewHead {
		t.Errorf("Labeling = %+v, want reworked_to_land true by %s", got, ruleReworkNewHead)
	}
}

// The first landing after the refusal decides: a later one cannot overturn it.
func TestReworkLabelsTheFirstLandingAfterTheRefusal(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	landedWith(t, town, "gt-abc", "999999aaaaaa", refusedAt.Add(3*time.Hour))
	landedWith(t, town, "gt-abc", refusedHead, refusedAt.Add(9*time.Hour))

	got := labelRework(t, town, reworkLine(t, "gt-abc", refusedHead))

	if !got.Labeled || got.Labels[qReworkedToLand] != true || got.Rule != ruleReworkNewHead {
		t.Errorf("Labeling = %+v, want the first landing to decide", got)
	}
}

// A bead that has not landed yet is unlabeled: whether it needs a rework
// commit is still open.
func TestReworkLeavesAnUnlandedBeadUnlabeled(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A landing of another bead, and one of this bead before the refusal, are
	// not the landing the label waits for.
	landedWith(t, town, "gt-other", refusedHead, refusedAt.Add(time.Hour))
	landedWith(t, town, "gt-abc", refusedHead, refusedAt.Add(-time.Hour))

	got := labelRework(t, town, reworkLine(t, "gt-abc", refusedHead))

	if got.Labeled {
		t.Errorf("Labeling = %+v, want unlabeled", got)
	}
}

// A landing between the refusal and the pass is still the one that decides:
// the label counts from when the refusal happened, not from when Jev answered.
func TestReworkLabelsFromTheRefusalNotTheAnswer(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	landedWith(t, town, "gt-abc", refusedHead, refusedAt.Add(5*time.Minute))

	got := labelRework(t, town, reworkLine(t, "gt-abc", refusedHead))

	if !got.Labeled || got.Labels[qReworkedToLand] != false || got.Rule != ruleReworkSameHead {
		t.Errorf("Labeling = %+v, want a landing before the answer to label it", got)
	}
}

// A refusal whose rejection block named no head is logged and unlabeled: no
// landing can be compared with it.
func TestReworkLeavesAHeadlessRefusalUnlabeled(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	landedWith(t, town, "gt-abc", refusedHead, refusedAt.Add(time.Hour))

	got := labelRework(t, town, reworkLine(t, "gt-abc", ""))

	if got.Labeled {
		t.Errorf("Labeling = %+v, want unlabeled", got)
	}
}

// The registered factory builds an adapter over the town's own files, and the
// battery is named in the registered list.
func TestReworkBatteryIsRegistered(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	landedWith(t, town, "gt-abc", refusedHead, refusedAt.Add(time.Hour))

	a, err := Open(ReworkBattery, town)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	it, err := a.Parse(reworkLine(t, "gt-abc", refusedHead))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := a.Label(context.Background(), it)
	if err != nil {
		t.Fatalf("Label: %v", err)
	}
	if !got.Labeled || got.Labels[qReworkedToLand] != false {
		t.Errorf("Labeling = %+v, want the same head's landing to label it", got)
	}
}

// A line from another question set, or without the fields a label is keyed
// by, cannot be compared with this one and is skipped by Parse.
func TestReworkParseRefusesUnlabelableLines(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"another version": func() []byte {
			var l rework.Line
			_ = json.Unmarshal(reworkLine(t, "gt-abc", refusedHead), &l)
			l.QuestionVersion = rework.Version + 1
			raw, _ := json.Marshal(l)
			return raw
		}(),
		"no refused time": func() []byte {
			var l rework.Line
			_ = json.Unmarshal(reworkLine(t, "gt-abc", refusedHead), &l)
			l.Refused = time.Time{}
			raw, _ := json.Marshal(l)
			return raw
		}(),
		"no bead":  reworkLine(t, "", refusedHead),
		"not json": []byte("{"),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := newReworkAdapter(rework.LogPath(t.TempDir()), t.TempDir())
			if _, err := a.Parse(raw); err == nil {
				t.Errorf("Parse accepted a line with %s", name)
			}
		})
	}
}

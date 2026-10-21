package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFixture writes the fixture log and its labels under town. An item with
// no entry in labels is unlabeled.
func writeFixture(t *testing.T, town string, lines []string, labels map[string]map[QuestionID]bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(FixtureLogPath(town)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(FixtureLogPath(town), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(FixtureLabelsPath(town), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func buildFixture(t *testing.T, town string, since time.Time) Report {
	t.Helper()
	a, err := Open(FixtureBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Build(context.Background(), a, since)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func question(t *testing.T, rep Report, id string) QuestionReport {
	t.Helper()
	for _, q := range rep.Questions {
		if q.ID == id {
			return q
		}
	}
	t.Fatalf("report has no question %q: %+v", id, rep.Questions)
	return QuestionReport{}
}

// fixtureLines holds four items that answer a noul, a choice and a score, and
// one line that does not parse.
var fixtureLines = []string{
	`{"ts":"2026-10-18T09:00:00Z","id":"fx-1","answers":{"needs":{"noul":0.9},"route":{"choice":"yes","probabilities":{"yes":0.8,"no":0.2},"positive":"yes"},"urgency":{"score":3}}}`,
	`{"ts":"2026-10-18T10:00:00Z","id":"fx-2","answers":{"needs":{"noul":0.2},"route":{"choice":"no","probabilities":{"yes":0.3,"no":0.7},"positive":"yes"},"urgency":{"score":0}}}`,
	`{"ts":"2026-10-18T11:00:00Z","id":"fx-3","answers":{"needs":{"noul":0.7},"route":{"choice":"yes","probabilities":{"yes":0.6,"no":0.4},"positive":"yes"},"urgency":{"score":1}}}`,
	`{"ts":"2026-10-18T12:00:00Z","id":"fx-4","answers":{"needs":{"noul":0.6},"route":{"choice":"no","probabilities":{"yes":0.4,"no":0.6},"positive":"yes"},"urgency":{"score":2}}}`,
	`not json`,
}

var fixtureLabels = map[string]map[QuestionID]bool{
	"fx-1": {"needs": true, "route": true, "urgency": true},
	"fx-2": {"needs": false, "route": false, "urgency": false},
	"fx-3": {"needs": false, "route": false, "urgency": false},
	// fx-4 has no label entry, so it is logged but not labeled.
}

func TestScorecardBuildCountsAndMetrics(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeFixture(t, town, fixtureLines, fixtureLabels)

	rep := buildFixture(t, town, time.Time{})
	if rep.Items != 4 || rep.LinesSkipped != 1 {
		t.Fatalf("items %d skipped %d, want 4 and 1", rep.Items, rep.LinesSkipped)
	}

	noul := question(t, rep, "needs")
	if noul.Kind != KindNoul || noul.Logged != 4 || noul.Labeled != 3 {
		t.Fatalf("needs: kind %s logged %d labeled %d, want noul 4 and 3", noul.Kind, noul.Logged, noul.Labeled)
	}
	if noul.Positives != 1 || noul.Negatives != 2 {
		t.Errorf("needs positives %d negatives %d, want 1 and 2", noul.Positives, noul.Negatives)
	}
	// At 0.5 fx-1 is a true yes and fx-3 a false one: precision 1 of 2, recall 1 of 1.
	if noul.Precision == nil || *noul.Precision != 0.5 || noul.Recall == nil || *noul.Recall != 1 {
		t.Errorf("needs precision %v recall %v, want 0.5 and 1", noul.Precision, noul.Recall)
	}
	if noul.BestF1 == nil || *noul.BestF1 != 1 || noul.BestThreshold == nil || *noul.BestThreshold != 0.9 {
		t.Errorf("needs best F1 %v at %v, want 1 at 0.9", noul.BestF1, noul.BestThreshold)
	}
	if len(noul.Calibration) != nBins {
		t.Fatalf("needs has %d calibration bins, want %d", len(noul.Calibration), nBins)
	}
	// 0.2 is in band 0.2-0.4, 0.7 in 0.6-0.8, 0.9 in 0.8-1.0.
	if got := noul.Calibration[1].N; got != 1 {
		t.Errorf("band 0.2-0.4 holds %d labeled lines, want 1", got)
	}
	if got := noul.Calibration[4]; got.N != 1 || got.LabelRate == nil || *got.LabelRate != 1 {
		t.Errorf("band 0.8-1.0 = %+v, want one labeled line with label rate 1", got)
	}
	if noul.Verdict != VerdictNotYet {
		t.Errorf("needs verdict %s, want not-yet on 3 labeled lines", noul.Verdict)
	}

	choice := question(t, rep, "route")
	if choice.Kind != KindChoice || choice.Logged != 4 || choice.Labeled != 3 {
		t.Fatalf("route: kind %s logged %d labeled %d, want choice 4 and 3", choice.Kind, choice.Logged, choice.Labeled)
	}
	// The chosen option is yes for fx-1 and fx-3; only fx-1 is a true yes.
	if choice.Precision == nil || *choice.Precision != 0.5 || choice.Recall == nil || *choice.Recall != 1 {
		t.Errorf("route precision %v recall %v, want 0.5 and 1", choice.Precision, choice.Recall)
	}
	if choice.BestThreshold == nil || *choice.BestThreshold != 0.8 {
		t.Errorf("route best threshold %v, want 0.8", choice.BestThreshold)
	}

	score := question(t, rep, "urgency")
	if score.Kind != KindScore || score.Logged != 4 || score.Labeled != 3 {
		t.Fatalf("urgency: kind %s logged %d labeled %d, want score 4 and 3", score.Kind, score.Logged, score.Labeled)
	}
	if score.MeanScorePositive == nil || *score.MeanScorePositive != 3 {
		t.Errorf("urgency mean score for yes = %v, want 3", score.MeanScorePositive)
	}
	if score.MeanScoreNegative == nil || *score.MeanScoreNegative != 0.5 {
		t.Errorf("urgency mean score for no = %v, want 0.5", score.MeanScoreNegative)
	}
	if score.Calibration != nil || score.Precision != nil {
		t.Errorf("urgency has calibration %v and precision %v, want neither", score.Calibration, score.Precision)
	}
}

func TestScorecardBuildSinceDropsOlderItems(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeFixture(t, town, fixtureLines, fixtureLabels)

	since := time.Date(2026, 10, 18, 10, 30, 0, 0, time.UTC)
	rep := buildFixture(t, town, since)
	if rep.Items != 2 {
		t.Fatalf("items = %d after since, want 2 (fx-3 and fx-4)", rep.Items)
	}
	if rep.Since == nil || !rep.Since.Equal(since) {
		t.Errorf("since = %v, want %v", rep.Since, since)
	}
}

func TestScorecardBuildWithNoLogIsEmpty(t *testing.T) {
	t.Parallel()
	rep := buildFixture(t, t.TempDir(), time.Time{})
	if rep.Items != 0 || len(rep.Questions) != 0 {
		t.Errorf("empty town gave items %d and %d questions, want none", rep.Items, len(rep.Questions))
	}
	// A dashboard reads the JSON, so no questions must be [], not null.
	if rep.Questions == nil {
		t.Error("empty report has nil Questions, which marshals to null")
	}
}

func TestScorecardOpenUnknownBatteryListsRegistered(t *testing.T) {
	t.Parallel()
	_, err := Open("no-such-battery", t.TempDir())
	if err == nil {
		t.Fatal("Open of an unknown battery succeeded")
	}
	if !strings.Contains(err.Error(), "registered batteries: attention, dispatch, finding, fixture, landing, progress, submission, targets") {
		t.Errorf("error %q does not list the registered batteries", err)
	}
}

// A label that cannot be read fails the whole report, rather than quietly
// leaving the item unlabeled.
type failingLabels struct{ fixture }

func (failingLabels) Label(context.Context, Item) (Labeling, error) {
	return Labeling{}, errors.New("bead store unavailable")
}

func TestScorecardBuildFailsWhenLabelingFails(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeFixture(t, town, fixtureLines, fixtureLabels)

	_, err := Build(context.Background(), failingLabels{fixture{town: town}}, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "bead store unavailable") {
		t.Fatalf("Build error = %v, want the labeling failure", err)
	}
}

// Forty labeled lines that separate perfectly give a best F1 of 1 against a
// base rate of one half, so the question earns.
func TestScorecardEarningOnSeparatedLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var lines []string
	labels := map[string]map[QuestionID]bool{}
	for i := range 40 {
		id := fmt.Sprintf("sep-%02d", i)
		v := "0.9"
		if i%2 == 1 {
			v = "0.1"
		}
		lines = append(lines, fmt.Sprintf(`{"ts":"2026-10-18T09:%02d:00Z","id":%q,"answers":{"needs":{"noul":%s}}}`, i, id, v))
		labels[id] = map[QuestionID]bool{"needs": i%2 == 0}
	}
	writeFixture(t, town, lines, labels)

	needs := question(t, buildFixture(t, town, time.Time{}), "needs")
	if needs.Labeled != 40 || needs.Verdict != VerdictEarning {
		t.Errorf("needs labeled %d verdict %s, want 40 and earning", needs.Labeled, needs.Verdict)
	}
}

func TestScorecardVerdictBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		labeled int
		bestF1  float64
		base    float64
		want    string
	}{
		{"one line short of 30 is not-yet even when F1 is perfect", 29, 1, 0.5, VerdictNotYet},
		{"exactly 30 with the margin met earns, though 0.6 - 0.5 is not exactly 0.1", 30, 0.6, 0.5, VerdictEarning},
		{"exactly 30 with the margin just missed is no-signal", 30, 0.5999, 0.5, VerdictNoSignal},
		{"an F1 equal to the base rate is no-signal", 30, 0.5, 0.5, VerdictNoSignal},
		{"a margin of 0.15 earns on 100 lines", 100, 0.95, 0.8, VerdictEarning},
		{"a margin of 0.1 on 100 lines earns, though 0.85 - 0.75 is not exactly 0.1", 100, 0.85, 0.75, VerdictEarning},
		{"a margin just under 0.1 on 100 lines is no-signal", 100, 0.8499, 0.75, VerdictNoSignal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := verdict(tc.labeled, tc.bestF1, tc.base); got != tc.want {
				t.Errorf("verdict(%d, %v, %v) = %s, want %s", tc.labeled, tc.bestF1, tc.base, got, tc.want)
			}
		})
	}
}

func TestScorecardBestThresholdKeepsHigherCutOnTie(t *testing.T) {
	t.Parallel()
	// With no positive label every cut has F1 0, so the highest value wins.
	thr, f1, ok := bestThreshold([]sample{{value: 0.9}, {value: 0.1}})
	if !ok || f1 != 0 || thr != 0.9 {
		t.Errorf("bestThreshold = (%v, %v, %v), want (0.9, 0, true)", thr, f1, ok)
	}
	if _, _, ok := bestThreshold(nil); ok {
		t.Error("bestThreshold of no samples reported a threshold")
	}
}

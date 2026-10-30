package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// writeFixture writes the fixture log and its labels under town. An item with
// no entry in labels is unlabeled.
func writeFixture(t *testing.T, town string, lines []string, labels map[string]map[QuestionID]bool) {
	t.Helper()
	if err := WriteFixture(town, lines, labels); err != nil {
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

func TestScorecardBuildCountsAndMetrics(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeFixture(t, town, FixtureLines, FixtureLabels)

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
	writeFixture(t, town, FixtureLines, FixtureLabels)

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
	if !strings.Contains(err.Error(), "registered batteries: attention, conflict, dispatch, drift, failcause, finding, landing, logtriage, notes, progress, rework, rules, stale, submission, targets, testsel, thrash") {
		t.Errorf("error %q does not list the registered batteries", err)
	}
	if strings.Contains(err.Error(), FixtureBattery) {
		t.Errorf("error %q lists the test-only %q battery", err, FixtureBattery)
	}
}

// The fixture battery opens by name for the tests that drive it, and the
// battery listings leave it out.
func TestRegisterTestOnlyOpensButIsNotListed(t *testing.T) {
	t.Parallel()
	if _, err := Open(FixtureBattery, t.TempDir()); err != nil {
		t.Fatalf("Open(%q): %v", FixtureBattery, err)
	}
	for _, name := range Registered() {
		if name == FixtureBattery {
			t.Errorf("Registered lists the test-only %q battery", name)
		}
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
	writeFixture(t, town, FixtureLines, FixtureLabels)

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
	prec := func(v float64) *float64 { return &v }
	cases := []struct {
		name      string
		kind      string
		positives int
		negatives int
		labeled   int
		bestF1    float64
		base      float64
		precision *float64
		flagged   int
		want      string
	}{
		{"one line short of 30 is not-yet even when F1 is perfect", KindNoul, 14, 15, 29, 1, 0.5, prec(1), 10, VerdictNotYet},
		{"exactly 30 with the margin met earns, though 0.6 - 0.5 is not exactly 0.1", KindNoul, 15, 15, 30, 0.6, 0.5, prec(0.6), 10, VerdictEarning},
		{"exactly 30 with the margin just missed is no-signal", KindNoul, 15, 15, 30, 0.5999, 0.5, prec(0.6), 20, VerdictNoSignal},
		{"an F1 equal to the base rate is no-signal", KindNoul, 15, 15, 30, 0.5, 0.5, prec(0.5), 20, VerdictNoSignal},
		{"a margin of 0.15 earns on 100 lines", KindNoul, 50, 50, 100, 0.95, 0.8, prec(0.95), 10, VerdictEarning},
		{"a margin of 0.1 on 100 lines earns, though 0.85 - 0.75 is not exactly 0.1", KindNoul, 50, 50, 100, 0.85, 0.75, prec(0.8), 10, VerdictEarning},
		{"a margin just under 0.1 on 100 lines is no-signal", KindNoul, 50, 50, 100, 0.8499, 0.75, prec(0.8), 20, VerdictNoSignal},
		{"nine positives is not-yet however well the metrics read", KindNoul, 9, 21, 30, 1, 0.3, prec(1), 10, VerdictNotYet},
		{"ten positives is not held back", KindNoul, 10, 20, 30, 0.9, 0.33, prec(0.9), 10, VerdictEarning},
		{"nine negatives is not-yet however well the metrics read", KindNoul, 21, 9, 30, 1, 0.7, prec(1), 10, VerdictNotYet},
		{"ten negatives is not held back", KindNoul, 20, 10, 30, 0.9, 0.67, prec(0.9), 10, VerdictEarning},
		{"precision just under 0.3 at the best cut is no-signal", KindNoul, 20, 20, 40, 1, 0.5, prec(0.2999), 20, VerdictNoSignal},
		{"precision exactly 0.3 at the best cut earns", KindNoul, 20, 20, 40, 1, 0.5, prec(0.3), 10, VerdictEarning},
		{"a score with nine positives is not-yet", KindScore, 9, 21, 30, 1, 0.3, nil, 0, VerdictNotYet},
		{"a score with ten positives is not held back", KindScore, 10, 20, 30, 0.9, 0.3, nil, 0, VerdictEarning},
		{"a score with nine negatives is not-yet", KindScore, 21, 9, 30, 1, 0.7, nil, 0, VerdictNotYet},
		{"a score with ten negatives skips the precision rule and earns", KindScore, 20, 10, 30, 0.9, 0.6, nil, 0, VerdictEarning},
		{"a score ignores a precision handed to it, since a score has none", KindScore, 20, 20, 40, 1, 0.5, prec(0.1), 0, VerdictEarning},
		{"a noul with no precision to read is no-signal, not judged on the margin alone", KindNoul, 20, 20, 40, 1, 0.5, nil, 0, VerdictNoSignal},
		{"a choice with no precision to read is no-signal, not judged on the margin alone", KindChoice, 20, 20, 40, 1, 0.5, nil, 0, VerdictNoSignal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := verdict(tc.kind, tc.positives, tc.negatives, tc.labeled, tc.bestF1, tc.base, tc.precision, tc.flagged); got != tc.want {
				t.Errorf("verdict(%s, %d positives, %d negatives, %d labeled, F1 %v, base %v, precision %s, %d flagged) = %s, want %s",
					tc.kind, tc.positives, tc.negatives, tc.labeled, tc.bestF1, tc.base, num(tc.precision), tc.flagged, got, tc.want)
			}
		})
	}
}

// A precision is an anecdote when the best cut flags too few labeled items to
// measure it on: finding.report_only measured 1.0 over one flagged item and
// read earning, though 1 of 14 positives found is not a signal (gt-dx8t2).
func TestVerdictFlaggedFloor(t *testing.T) {
	t.Parallel()
	prec := func(v float64) *float64 { return &v }
	// The finding.report_only line as measured on 2026-10-30, with only the
	// count at the best cut moving between the rows.
	const (
		positives = 14
		negatives = 695
		labeled   = positives + negatives
		bestF1    = 0.133
		baseRate  = 0.0197
	)
	cases := []struct {
		name      string
		kind      string
		precision *float64
		flagged   int
		want      string
	}{
		{"precision 1.0 over one flagged item is an anecdote", KindNoul, prec(1), 1, VerdictNotYet},
		{"ten flagged items at precision 1.0 earns", KindNoul, prec(1), minFlaggedAtBest, VerdictEarning},
		{"nine flagged items is one short of the floor", KindNoul, prec(1), minFlaggedAtBest - 1, VerdictNotYet},
		{"a choice is held to the same floor", KindChoice, prec(1), 1, VerdictNotYet},
		{"a score has no cut to flag with, so the floor does not apply", KindScore, nil, 0, VerdictEarning},
		{"a question with no best cut to read stays no-signal", KindNoul, nil, 0, VerdictNoSignal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := verdict(tc.kind, positives, negatives, labeled, bestF1, baseRate, tc.precision, tc.flagged); got != tc.want {
				t.Errorf("verdict(%s, %d positives, %d negatives, %d labeled, F1 %v, base %v, precision %s, %d flagged) = %s, want %s",
					tc.kind, positives, negatives, labeled, bestF1, baseRate, num(tc.precision), tc.flagged, got, tc.want)
			}
		})
	}
}

// finding.any_duplicate read earning on 615 labeled lines with 3 positives, so
// a log shaped like it must read not-yet, even though its best F1 clears the
// margin against the base rate.
func TestScorecardVerdictRarePositivesStayNotYet(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var lines []string
	labels := map[string]map[QuestionID]bool{}
	for i := range 600 {
		id := fmt.Sprintf("rare-%03d", i)
		value, label := "0.1", false
		if i < 3 {
			value, label = "0.9", true
		}
		lines = append(lines, fmt.Sprintf(`{"ts":"2026-10-18T09:00:00Z","id":%q,"answers":{"needs":{"noul":%s}}}`, id, value))
		labels[id] = map[QuestionID]bool{"needs": label}
	}
	writeFixture(t, town, lines, labels)

	needs := question(t, buildFixture(t, town, time.Time{}), "needs")
	if needs.Labeled != 600 || needs.Positives != 3 || needs.Negatives != 597 {
		t.Fatalf("needs labeled %d positives %d negatives %d, want 600, 3 and 597", needs.Labeled, needs.Positives, needs.Negatives)
	}
	if needs.BestF1 == nil || needs.BaseRate == nil || *needs.BestF1-*needs.BaseRate < earnMargin {
		t.Fatalf("fixture does not clear the F1 margin, so not-yet would say nothing: F1 %s base %s", num(needs.BestF1), num(needs.BaseRate))
	}
	if needs.Verdict != VerdictNotYet {
		t.Errorf("needs verdict = %s, want not-yet on 3 positives", needs.Verdict)
	}
}

// Enough positives can still answer yes wrongly most of the time: at the best
// cut this log is 20 true against 180 false, so it reads no-signal.
func TestScorecardVerdictPrecisionFloorHoldsEarningBack(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var lines []string
	labels := map[string]map[QuestionID]bool{}
	for i := range 600 {
		id := fmt.Sprintf("noisy-%03d", i)
		// 20 positives and 180 negatives share the top value, so the best cut
		// carries 200 yes answers of which 20 are right; the rest rank low.
		value, label := "0.1", false
		if i < 200 {
			value = "0.9"
		}
		if i < 20 {
			label = true
		}
		lines = append(lines, fmt.Sprintf(`{"ts":"2026-10-18T09:00:00Z","id":%q,"answers":{"needs":{"noul":%s}}}`, id, value))
		labels[id] = map[QuestionID]bool{"needs": label}
	}
	writeFixture(t, town, lines, labels)

	needs := question(t, buildFixture(t, town, time.Time{}), "needs")
	if needs.Positives != 20 || needs.Negatives != 580 {
		t.Fatalf("needs positives %d negatives %d, want 20 and 580", needs.Positives, needs.Negatives)
	}
	if needs.BestF1 == nil || needs.BaseRate == nil || *needs.BestF1-*needs.BaseRate < earnMargin {
		t.Fatalf("fixture does not clear the F1 margin, so no-signal would not show the precision rule: F1 %s base %s", num(needs.BestF1), num(needs.BaseRate))
	}
	if needs.PrecisionAtBest == nil || *needs.PrecisionAtBest != 0.1 {
		t.Fatalf("precision_at_best = %s, want 0.1", num(needs.PrecisionAtBest))
	}
	if needs.Verdict != VerdictNoSignal {
		t.Errorf("needs verdict = %s, want no-signal at 0.1 precision", needs.Verdict)
	}
}

// A score has no precision, so the precision floor never holds it back: 20
// positives and 20 negatives on a separated score log earn.
func TestScorecardVerdictScoreKindSkipsPrecisionRule(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var lines []string
	labels := map[string]map[QuestionID]bool{}
	for i := range 40 {
		id := fmt.Sprintf("level-%02d", i)
		score := 0
		if i%2 == 0 {
			score = 3
		}
		lines = append(lines, fmt.Sprintf(`{"ts":"2026-10-18T09:%02d:00Z","id":%q,"answers":{"level":{"score":%d}}}`, i, id, score))
		labels[id] = map[QuestionID]bool{"level": i%2 == 0}
	}
	writeFixture(t, town, lines, labels)

	level := question(t, buildFixture(t, town, time.Time{}), "level")
	if level.Labeled != 40 || level.Positives != 20 || level.Negatives != 20 {
		t.Fatalf("level labeled %d positives %d negatives %d, want 40, 20 and 20", level.Labeled, level.Positives, level.Negatives)
	}
	if level.PrecisionAtBest != nil {
		t.Errorf("a score question printed precision_at_best %s", num(level.PrecisionAtBest))
	}
	if level.Verdict != VerdictEarning {
		t.Errorf("level verdict = %s, want earning on 20 positives and 20 negatives", level.Verdict)
	}
}

// precision_at_best is printed where there is a best cut to read and a
// precision to read there, and the key it adds moves none of the keys the
// report already printed.
func TestScorecardPrecisionAtBest(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	lines := []string{
		`{"ts":"2026-10-18T09:00:00Z","id":"pb-1","answers":{"graded":{"noul":0.9},"ungraded":{"noul":0.8},"level":{"score":3}}}`,
		`{"ts":"2026-10-18T09:01:00Z","id":"pb-2","answers":{"graded":{"noul":0.2},"ungraded":{"noul":0.1},"level":{"score":1}}}`,
	}
	labels := map[string]map[QuestionID]bool{
		"pb-1": {"graded": true, "level": true},
		"pb-2": {"graded": false, "level": false},
	}
	writeFixture(t, town, lines, labels)

	data, err := json.Marshal(buildFixture(t, town, time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Questions []map[string]any `json:"questions"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("decoding the JSON report: %v\n%s", err, data)
	}
	byID := map[string]map[string]any{}
	for _, q := range rep.Questions {
		id, _ := q["id"].(string)
		byID[id] = q
	}

	graded := byID["graded"]
	if graded == nil {
		t.Fatalf("report has no graded question: %s", data)
	}
	// The best cut is 0.9, where one labeled line is predicted yes and is right.
	got, ok := graded["precision_at_best"]
	if !ok || got != 1.0 {
		t.Errorf("graded precision_at_best = %v (present %v), want 1\n%s", got, ok, data)
	}
	for _, key := range []string{"id", "kind", "logged", "labeled", "positives", "negatives", "base_rate", "precision", "recall", "best_threshold", "best_f1", "verdict"} {
		if _, ok := graded[key]; !ok {
			t.Errorf("graded question no longer prints the %q key: %s", key, data)
		}
	}
	for _, id := range []string{"level", "ungraded"} {
		if q := byID[id]; q == nil {
			t.Errorf("report has no %q question: %s", id, data)
		} else if v, ok := q["precision_at_best"]; ok {
			t.Errorf("%s question printed precision_at_best %v, want it absent", id, v)
		}
	}
}

// flagged_at_best is how many labeled items the precision at the best cut was
// measured on: it prints in the JSON and the text for a noul and a choice, and
// a score question carries neither.
func TestScorecardFlaggedAtBest(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	lines := []string{
		`{"ts":"2026-10-18T09:00:00Z","id":"fb-1","answers":{"graded":{"noul":0.9},"route":{"choice":"yes","probabilities":{"yes":0.9,"no":0.1},"positive":"yes"},"level":{"score":3}}}`,
		`{"ts":"2026-10-18T09:01:00Z","id":"fb-2","answers":{"graded":{"noul":0.2},"route":{"choice":"no","probabilities":{"yes":0.2,"no":0.8},"positive":"yes"},"level":{"score":1}}}`,
		`{"ts":"2026-10-18T09:02:00Z","id":"fb-3","answers":{"graded":{"noul":0.3},"route":{"choice":"no","probabilities":{"yes":0.3,"no":0.7},"positive":"yes"},"level":{"score":1}}}`,
	}
	labels := map[string]map[QuestionID]bool{
		"fb-1": {"graded": true, "route": true, "level": true},
		"fb-2": {"graded": false, "route": false, "level": false},
		"fb-3": {"graded": false, "route": true, "level": false},
	}
	writeFixture(t, town, lines, labels)
	rep := buildFixture(t, town, time.Time{})

	// graded's best cut is 0.9, which flags fb-1 alone; route's is 0.3, which
	// flags fb-1 and fb-3.
	if graded := question(t, rep, "graded"); graded.FlaggedAtBest != 1 {
		t.Errorf("graded flagged_at_best = %d, want 1", graded.FlaggedAtBest)
	}
	if route := question(t, rep, "route"); route.FlaggedAtBest != 2 {
		t.Errorf("route flagged_at_best = %d, want 2", route.FlaggedAtBest)
	}
	if level := question(t, rep, "level"); level.FlaggedAtBest != 0 {
		t.Errorf("level flagged_at_best = %d, want 0 on a score", level.FlaggedAtBest)
	}

	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Questions []map[string]any `json:"questions"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decoding the JSON report: %v\n%s", err, data)
	}
	byID := map[string]map[string]any{}
	for _, q := range decoded.Questions {
		id, _ := q["id"].(string)
		byID[id] = q
	}
	for id, want := range map[string]float64{"graded": 1, "route": 2} {
		if got, ok := byID[id]["flagged_at_best"]; !ok || got != want {
			t.Errorf("%s flagged_at_best = %v (present %v), want %v\n%s", id, got, ok, want, data)
		}
	}
	if got, ok := byID["level"]["flagged_at_best"]; ok {
		t.Errorf("a score question printed flagged_at_best %v, want it absent", got)
	}
	if _, ok := byID["level"]["precision_at_best"]; ok {
		t.Error("a score question printed precision_at_best, want it absent")
	}

	var out strings.Builder
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"precision at the best F1 cut: 100.0% over 1 flagged", "precision at the best F1 cut: 100.0% over 2 flagged"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("text report has no %q:\n%s", want, out.String())
		}
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

// A missing log is still an empty report, but the report says so, so it is
// not read as a battery that logged nothing.
func TestScorecardBuildFlagsMissingLog(t *testing.T) {
	t.Parallel()
	missing := buildFixture(t, t.TempDir(), time.Time{})
	if !missing.LogMissing {
		t.Error("a missing log is not flagged LogMissing")
	}
	var out strings.Builder
	if err := WriteText(&out, missing); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No log yet") {
		t.Errorf("text report of a missing log does not say so:\n%s", out.String())
	}

	town := t.TempDir()
	writeFixture(t, town, FixtureLines, FixtureLabels)
	if buildFixture(t, town, time.Time{}).LogMissing {
		t.Error("a present log is flagged LogMissing")
	}
}

// A label whose item has no usable answer for the question is counted, not
// dropped: here one item answers in the wrong shape and one has no answer.
func TestScorecardCountsLabelsWithoutUsableAnswer(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	lines := []string{
		`{"ts":"2026-10-18T09:00:00Z","id":"a","answers":{"needs":{"noul":0.9}}}`,
		`{"ts":"2026-10-18T10:00:00Z","id":"b","answers":{"needs":{"choice":"yes","probabilities":{"yes":0.5},"positive":"yes"}}}`,
		`{"ts":"2026-10-18T11:00:00Z","id":"c","answers":{"route":{"noul":0.4}}}`,
		`{"ts":"2026-10-18T12:00:00Z","id":"d","answers":{"needs":{"noul":0.2}}}`,
	}
	labels := map[string]map[QuestionID]bool{
		"a": {"needs": true},
		"b": {"needs": true},
		"c": {"needs": true},
		"d": {"needs": false},
	}
	writeFixture(t, town, lines, labels)

	rep := buildFixture(t, town, time.Time{})
	q := question(t, rep, "needs")
	if q.Labeled != 2 || q.Unmatched != 2 {
		t.Errorf("needs labeled %d, unmatched %d; want labeled 2, unmatched 2 (b wrong shape, c no answer)", q.Labeled, q.Unmatched)
	}
	if q.Logged != 2 {
		t.Errorf("needs logged %d, want 2: an unusable answer is not logged", q.Logged)
	}
}

// The fixture's labels file is part of the setup, so a missing one is a
// failure, not a set of unlabeled items.
func TestScorecardFixtureMissingLabelsFails(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeFixture(t, town, FixtureLines, FixtureLabels)
	if err := os.Remove(FixtureLabelsPath(town)); err != nil {
		t.Fatal(err)
	}
	a, err := Open(FixtureBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), a, time.Time{}); err == nil || !strings.Contains(err.Error(), "fixture labels") {
		t.Fatalf("Build error = %v, want the missing fixture labels", err)
	}
}

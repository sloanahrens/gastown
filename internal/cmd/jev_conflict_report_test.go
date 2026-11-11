package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/conflict"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
	"github.com/steveyegge/gastown/internal/land"
)

// jevConflictNow is the report's clock. The 14-day window it reads opens at
// 2026-10-28 12:00 UTC.
var jevConflictNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

var jevConflictQSameFiles = scorecard.QuestionID(conflict.QuestionSameFiles)

// jevConflictLabeler is the conflict adapter with its labels replaced. Parse is
// the real adapter's, so a log line reads exactly as the scorecard reads it;
// Label returns the labels the test gives by item ID, or readErr.
type jevConflictLabeler struct {
	scorecard.Adapter
	labels  map[string]scorecard.Labeling
	readErr error
}

func (l jevConflictLabeler) Label(_ context.Context, it scorecard.Item) (scorecard.Labeling, error) {
	if l.readErr != nil {
		return scorecard.Labeling{}, l.readErr
	}
	return l.labels[it.ID], nil
}

func jevConflictOpenWith(labels map[string]scorecard.Labeling, readErr error) jevConflictOpener {
	return func(battery, town string) (scorecard.Adapter, error) {
		a, err := scorecard.Open(battery, town)
		if err != nil {
			return nil, err
		}
		return jevConflictLabeler{Adapter: a, labels: labels, readErr: readErr}, nil
	}
}

// jevConflictLandingsAt puts fixed landing stamps behind the report: a bead in
// landed landed at its stamp. readErr, when set, fails every read.
func jevConflictLandingsAt(landed map[string]time.Time, readErr error) jevConflictLandingsOpener {
	return func(string) (jevLandedAt, error) {
		return func(bead string) (time.Time, bool, error) {
			if readErr != nil {
				return time.Time{}, false, readErr
			}
			at, ok := landed[bead]
			return at, ok, nil
		}, nil
	}
}

// jevConflictID is the adapter's item ID for a line written with bead first.
func jevConflictID(bead, other string) string { return bead + "+" + other }

// jevConflictLogLine is one conflict log line for bead placed with other,
// answered at at with the given same_files probability.
func jevConflictLogLine(t *testing.T, bead, other string, at time.Time, sameFiles float64) string {
	t.Helper()
	raw, err := json.Marshal(conflict.Line{
		TS:              at,
		Bead:            bead,
		AttachedAt:      at.Add(-time.Hour),
		Other:           other,
		OtherAttachedAt: at.Add(-2 * time.Hour),
		Answers:         conflict.Answers{SameFiles: sameFiles},
		QuestionVersion: conflict.Version,
		Model:           "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// writeJevConflictLog writes lines to the conflict log under town.
func writeJevConflictLog(t *testing.T, town string, lines ...string) {
	t.Helper()
	path := conflict.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runJevConflictJSON runs the report as --json over the default window and
// decodes it.
func runJevConflictJSON(t *testing.T, town string, open jevConflictOpener, landings jevConflictLandingsOpener) jevConflictReport {
	t.Helper()
	var out bytes.Buffer
	if err := jevConflictReportWith(context.Background(), &out, town, "", true, jevConflictNow, open, landings); err != nil {
		t.Fatal(err)
	}
	var rep jevConflictReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decoding the report: %v\n%s", err, out.String())
	}
	return rep
}

// jevConflictCutAt returns the report's block for cut, failing if it is absent.
func jevConflictCutAt(t *testing.T, rep jevConflictReport, cut float64) jevConflictCut {
	t.Helper()
	for _, c := range rep.Cuts {
		if c.Cut == cut {
			return c
		}
	}
	t.Fatalf("no block for cut %v in %+v", cut, rep.Cuts)
	return jevConflictCut{}
}

// jevConflictDayPairs maps each day in a cut's block to its pair count.
func jevConflictDayPairs(days []jevConflictDay) map[string]int {
	m := map[string]int{}
	for _, d := range days {
		m[d.Day] = d.Pairs
	}
	return m
}

func TestJevConflictReportCountsPairsPerDayAtEachCut(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeJevConflictLog(t, town,
		jevConflictLogLine(t, "a", "b", time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC), 0.9),
		jevConflictLogLine(t, "c", "d", time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC), 0.7),
		jevConflictLogLine(t, "e", "f", time.Date(2026, 11, 9, 11, 0, 0, 0, time.UTC), 0.5),
		jevConflictLogLine(t, "k", "l", time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), 0.6),
		jevConflictLogLine(t, "g", "h", time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), 0.85),
		jevConflictLogLine(t, "i", "j", time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), 0.95),
	)

	rep := runJevConflictJSON(t, town, jevConflictOpenWith(nil, nil), jevConflictLandingsAt(nil, nil))

	low := jevConflictCutAt(t, rep, 0.6)
	// A pair at exactly the cut is at it.
	if got, want := jevConflictDayPairs(low.Days), map[string]int{"2026-11-09": 3, "2026-11-10": 1}; !sameIntMap(got, want) {
		t.Errorf("cut 0.6 pairs per day = %v, want %v", got, want)
	}
	if low.Total.Pairs != 4 {
		t.Errorf("cut 0.6 total pairs = %d, want 4", low.Total.Pairs)
	}
	// The window is 14 days, so the mean is the four pairs over fourteen days.
	if got := low.Total.MeanPairsPerDay; !nearly(got, 4.0/14.0) {
		t.Errorf("cut 0.6 mean pairs per day = %v, want 4/14", got)
	}
	high := jevConflictCutAt(t, rep, 0.8)
	if got, want := jevConflictDayPairs(high.Days), map[string]int{"2026-11-09": 1, "2026-11-10": 1}; !sameIntMap(got, want) {
		t.Errorf("cut 0.8 pairs per day = %v, want %v", got, want)
	}
	if high.Total.Pairs != 2 {
		t.Errorf("cut 0.8 total pairs = %d, want 2", high.Total.Pairs)
	}
}

func sameIntMap(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestJevConflictReportCountsABeadPairOnce(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// x and y land at 10:00 and 11:00 on the 11th, so an answer counts only
	// when logged before 11:00 that day. The pair is logged three times, the
	// middle time with its beads reversed. Only its newest answer before the
	// landing counts; the last one, logged after both landed, does not.
	writeJevConflictLog(t, town,
		jevConflictLogLine(t, "x", "y", time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC), 0.9),
		jevConflictLogLine(t, "y", "x", time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), 0.7),
		jevConflictLogLine(t, "x", "y", time.Date(2026, 11, 11, 11, 30, 0, 0, time.UTC), 0.95),
	)
	landed := map[string]time.Time{
		"x": time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC),
		"y": time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC),
	}

	rep := runJevConflictJSON(t, town, jevConflictOpenWith(nil, nil), jevConflictLandingsAt(landed, nil))

	low := jevConflictCutAt(t, rep, 0.6)
	if low.Total.Pairs != 1 {
		t.Errorf("cut 0.6 total pairs = %d, want 1 for one bead pair", low.Total.Pairs)
	}
	if got, want := jevConflictDayPairs(low.Days), map[string]int{"2026-11-10": 1}; !sameIntMap(got, want) {
		t.Errorf("cut 0.6 pairs per day = %v, want the newest pre-landing answer's day %v", got, want)
	}
	if high := jevConflictCutAt(t, rep, 0.8); high.Total.Pairs != 0 {
		t.Errorf("cut 0.8 total pairs = %d, want 0: the counted answer is 0.7", high.Total.Pairs)
	}
}

func TestJevConflictReportDropsAPairAnsweredOnlyAfterLanding(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeJevConflictLog(t, town,
		jevConflictLogLine(t, "x", "y", time.Date(2026, 11, 11, 11, 30, 0, 0, time.UTC), 0.95),
	)
	landed := map[string]time.Time{
		"x": time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC),
		"y": time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC),
	}

	rep := runJevConflictJSON(t, town, jevConflictOpenWith(nil, nil), jevConflictLandingsAt(landed, nil))

	if rep.Pairs != 0 || jevConflictCutAt(t, rep, 0.6).Total.Pairs != 0 {
		t.Errorf("a pair answered only after both beads landed counted: pairs %d", rep.Pairs)
	}
}

func TestJevConflictReportCountsAPairWithAnUnlandedBead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeJevConflictLog(t, town,
		jevConflictLogLine(t, "x", "y", time.Date(2026, 11, 11, 11, 30, 0, 0, time.UTC), 0.95),
	)
	// Only x landed, so the pair has no cutoff and its answer counts.
	landed := map[string]time.Time{"x": time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)}

	rep := runJevConflictJSON(t, town, jevConflictOpenWith(nil, nil), jevConflictLandingsAt(landed, nil))

	if got, want := jevConflictDayPairs(jevConflictCutAt(t, rep, 0.6).Days), map[string]int{"2026-11-11": 1}; !sameIntMap(got, want) {
		t.Errorf("cut 0.6 pairs per day = %v, want %v", got, want)
	}
}

func TestJevConflictReportPrecisionAndIntervalOnAFixture(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	day := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	labels := map[string]scorecard.Labeling{}
	label := func(bead, other string, overlapped bool) {
		labels[jevConflictID(bead, other)] = scorecard.Labeling{
			Labeled: true, Labels: map[scorecard.QuestionID]bool{jevConflictQSameFiles: overlapped}, Rule: "test",
		}
	}
	var lines []string
	// Nine pairs at 0.85 on the 10th: eight labeled, six overlapped, one
	// unlabeled.
	for i, bead := range []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"} {
		lines = append(lines, jevConflictLogLine(t, bead, "q", day.Add(time.Duration(i)*time.Minute), 0.85))
		if i < 6 {
			label(bead, "q", true)
		} else if i < 8 {
			label(bead, "q", false)
		}
	}
	// One pair at 0.7 on the 9th, labeled and overlapped: in the 0.6 cut only.
	lines = append(lines, jevConflictLogLine(t, "r", "s", time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC), 0.7))
	label("r", "s", false)
	writeJevConflictLog(t, town, lines...)

	rep := runJevConflictJSON(t, town, jevConflictOpenWith(labels, nil), jevConflictLandingsAt(nil, nil))

	low := jevConflictCutAt(t, rep, 0.6)
	if low.Total.Pairs != 10 || low.Total.Labeled != 9 || low.Total.Overlapped != 6 {
		t.Errorf("cut 0.6 totals pairs/labeled/overlapped = %d/%d/%d, want 10/9/6",
			low.Total.Pairs, low.Total.Labeled, low.Total.Overlapped)
	}
	if low.Total.Precision == nil || !nearly(*low.Total.Precision, 6.0/9.0) {
		t.Errorf("cut 0.6 precision = %v, want 6/9", low.Total.Precision)
	}
	// Wilson 95% for 6 of 9.
	if low.Total.WilsonLow == nil || low.Total.WilsonHigh == nil ||
		!nearly3(*low.Total.WilsonLow, 0.354) || !nearly3(*low.Total.WilsonHigh, 0.879) {
		t.Errorf("cut 0.6 Wilson interval = %v..%v, want 0.354..0.879", low.Total.WilsonLow, low.Total.WilsonHigh)
	}

	high := jevConflictCutAt(t, rep, 0.8)
	if high.Total.Pairs != 9 || high.Total.Labeled != 8 || high.Total.Overlapped != 6 {
		t.Errorf("cut 0.8 totals pairs/labeled/overlapped = %d/%d/%d, want 9/8/6",
			high.Total.Pairs, high.Total.Labeled, high.Total.Overlapped)
	}
	if high.Total.Precision == nil || !nearly(*high.Total.Precision, 0.75) {
		t.Errorf("cut 0.8 precision = %v, want 0.75", high.Total.Precision)
	}
	if len(high.Days) != 1 || high.Days[0].Day != "2026-11-10" || high.Days[0].Labeled != 8 || high.Days[0].Overlapped != 6 {
		t.Errorf("cut 0.8 day rows = %+v, want one row for 2026-11-10 with 8 labeled and 6 overlapped", high.Days)
	}
}

// nearly and nearly3 compare a float to a value with a tolerance: 1e-9 for an
// exact fraction, and 0.0005 for a figure printed to three places.
func nearly(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func nearly3(got, want float64) bool { return math.Abs(got-want) < 0.0005 }

func TestJevConflictReportJSONShape(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeJevConflictLog(t, town,
		jevConflictLogLine(t, "a", "b", time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), 0.9),
		"not a conflict line",
	)

	var out bytes.Buffer
	if err := jevConflictReportWith(context.Background(), &out, town, "", true, jevConflictNow,
		jevConflictOpenWith(nil, nil), jevConflictLandingsAt(nil, nil)); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("decoding the report: %v\n%s", err, out.String())
	}
	requireKeys(t, "report", doc, "since", "window_days", "pairs", "lines_skipped", "cuts")
	cuts, ok := doc["cuts"].([]any)
	if !ok || len(cuts) != 2 {
		t.Fatalf("cuts = %v, want two blocks", doc["cuts"])
	}
	cut := cuts[0].(map[string]any)
	requireKeys(t, "cut", cut, "cut", "days", "total")
	days := cut["days"].([]any)
	if len(days) != 1 {
		t.Fatalf("cut 0.6 days = %v, want one day", days)
	}
	requireKeys(t, "day", days[0].(map[string]any), "day", "pairs", "labeled", "overlapped", "precision")
	total := cut["total"].(map[string]any)
	requireKeys(t, "total", total, "pairs", "labeled", "overlapped", "precision", "wilson_low", "wilson_high", "mean_pairs_per_day")
	// No pair is labeled, so the precision and its interval are null rather
	// than zero.
	for _, k := range []string{"precision", "wilson_low", "wilson_high"} {
		if total[k] != nil {
			t.Errorf("total %s = %v, want null with no labeled pair", k, total[k])
		}
	}
	if skipped := doc["lines_skipped"]; skipped != float64(1) {
		t.Errorf("lines_skipped = %v, want 1", skipped)
	}
}

// requireKeys fails the test for each key the object lacks. A key holding null
// is present.
func requireKeys(t *testing.T, what string, obj map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			t.Errorf("%s has no %q key: %v", what, k, obj)
		}
	}
}

func TestJevConflictReportLabelErrorFailsTheReport(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeJevConflictLog(t, town, jevConflictLogLine(t, "a", "b", time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), 0.9))

	var out bytes.Buffer
	err := jevConflictReportWith(context.Background(), &out, town, "", false, jevConflictNow,
		jevConflictOpenWith(nil, context.DeadlineExceeded), jevConflictLandingsAt(nil, nil))
	if err == nil {
		t.Fatalf("a label that cannot be read returned a report:\n%s", out.String())
	}
	if out.Len() != 0 {
		t.Errorf("a failed report wrote output: %q", out.String())
	}
}

func TestJevConflictReportLandingReadErrorFailsTheReport(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeJevConflictLog(t, town, jevConflictLogLine(t, "a", "b", time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), 0.9))

	var out bytes.Buffer
	err := jevConflictReportWith(context.Background(), &out, town, "", false, jevConflictNow,
		jevConflictOpenWith(nil, nil), jevConflictLandingsAt(nil, context.DeadlineExceeded))
	if err == nil {
		t.Fatalf("a landing that cannot be read returned a report:\n%s", out.String())
	}
}

// jevConflictLandingsPath is the rig's landings file under town, the one the
// production reader opens.
func jevConflictLandingsPath(t *testing.T, town string) string {
	t.Helper()
	f, err := land.RigLandingsFile(town, conflict.RigName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	return f.Path
}

func TestJevConflictLandingsReadsTheLandedStamp(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	landedAt := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	rec, err := json.Marshal(land.LandingRecord{BeadID: "x", LandedAt: landedAt})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jevConflictLandingsPath(t, town), append(rec, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	landed, err := jevConflictLandings(town)
	if err != nil {
		t.Fatal(err)
	}
	if at, ok, err := landed("x"); err != nil || !ok || !at.Equal(landedAt) {
		t.Errorf("landed(x) = %v, %v, %v; want %v, true, nil", at, ok, err, landedAt)
	}
	if _, ok, err := landed("y"); err != nil || ok {
		t.Errorf("landed(y) found = %v, err %v; want no record and no error", ok, err)
	}
}

func TestJevConflictLandingsFailsOnAnUnreadableFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory where the landings file should be opens but cannot be read.
	if err := os.MkdirAll(jevConflictLandingsPath(t, town), 0o755); err != nil {
		t.Fatal(err)
	}

	landed, err := jevConflictLandings(town)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := landed("x"); err == nil {
		t.Error("landed(x) read an unreadable landings file as no landing")
	}
}

func TestJevConflictReportMissingLogIsEmpty(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	rep := runJevConflictJSON(t, town, jevConflictOpenWith(nil, nil), jevConflictLandingsAt(nil, nil))

	if rep.Pairs != 0 || rep.LinesSkipped != 0 {
		t.Errorf("missing log reported pairs %d, skipped %d; want none", rep.Pairs, rep.LinesSkipped)
	}
}

func TestJevConflictReportRejectsABadSince(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := jevConflictReportWith(context.Background(), &out, t.TempDir(), "0d", false, jevConflictNow,
		jevConflictOpenWith(nil, nil), jevConflictLandingsAt(nil, nil))
	if err == nil {
		t.Fatal("--since 0d was accepted")
	}
}

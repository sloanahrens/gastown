package metrics

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// evalTableRow matches one row of the evaluation table, whatever column widths
// tabwriter chose: the predictor's cells in the order the header names them.
func evalTableRow(name string, cells ...string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^` + name + `\s+` + strings.Join(cells, `\s+`) + `$`)
}

// TestStoryPointsEvalPrinter: the hand-built figures of each predictor are
// printed in minutes, and a figure no row could produce is n/a rather than a
// zero. The join's counts are printed above the table.
func TestStoryPointsEvalPrinter(t *testing.T) {
	t.Parallel()
	counts := EvalCounts{Records: 50, Joined: 45, NoEstimate: 2, LowerBound: 1, UpperBound: 1, Absent: 1, MalformedLog: 3}
	stats := []PredictorStats{
		{Name: predictorBlended, N: 40, MAE: 2.5, MedianAE: 2, Within: 0.75, WithinOK: true, Rho: 0.5},
		{Name: predictorShape, N: 10, SkippedLog: 2, MAE: 12.25, MedianAE: 9.5, LogRMS: 0.693, LogBias: -0.2, Within: 0.5, WithinOK: true, RhoErr: ErrNoSpread},
		{Name: predictorJev, N: 0, RhoErr: errEvalNoRows},
	}
	var out bytes.Buffer
	if err := writeStoryPointsEval(&out, counts, 40, stats); err != nil {
		t.Fatalf("writeStoryPointsEval: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"story points evaluation: 40 test rows of 45 joined rows (50 landing records read)",
		"dropped: no estimate 2, lower bound 1, upper bound 1, absent 1; malformed log lines 3",
		"predictor",
		"log n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("out = %q, want it to hold %q", text, want)
		}
	}
	// Blended: n, MAE, median AE, within, rho, log n, log RMS, log bias.
	if want := evalTableRow("blended", "40", "2.50", "2.00", "0.75", "0.50", "40", "0.00", "0.00"); !want.MatchString(text) {
		t.Errorf("out = %q, want a blended row %v", text, want)
	}
	// Two rows are outside the log figures, and a correlation with no spread
	// in one series is n/a.
	if want := evalTableRow("shape", "10", "12.25", "9.50", "0.50", "n/a", "8", "0.69", "-0.20"); !want.MatchString(text) {
		t.Errorf("out = %q, want a shape row %v", text, want)
	}
	if want := evalTableRow("jev", "0", "n/a", "n/a", "n/a", "n/a", "0", "n/a", "n/a"); !want.MatchString(text) {
		t.Errorf("out = %q, want a jev row %v: a predictor with no test row has no figure", text, want)
	}
}

// TestStoryPointsEvalShortPart: a test part under storyPointsEvalMinTest rows
// prints the counts the join gives and says no figures are printed, even when
// the caller passes figures: the part, not the statistics, decides.
func TestStoryPointsEvalShortPart(t *testing.T) {
	t.Parallel()
	counts := EvalCounts{Records: 34, Joined: 29, NoEstimate: 2, LowerBound: 1, UpperBound: 1, Absent: 1, MalformedLog: 3}
	stats := []PredictorStats{{Name: predictorBlended, N: 29, MAE: 4, Within: 1, WithinOK: true}}
	var out bytes.Buffer
	if err := writeStoryPointsEval(&out, counts, storyPointsEvalMinTest-1, stats); err != nil {
		t.Fatalf("writeStoryPointsEval: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"story points evaluation: 29 test rows of 29 joined rows (34 landing records read)",
		"dropped: no estimate 2, lower bound 1, upper bound 1, absent 1; malformed log lines 3",
		"the test part is under 30 rows, so no error figures are printed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("out = %q, want it to hold %q", text, want)
		}
	}
	for _, unwanted := range []string{"predictor", "MAE"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("out = %q, want no error figures at %q", text, unwanted)
		}
	}
}

// TestStoryPointsEvalReportsAFailedWrite: a table the step cannot write fails
// the run, rather than reporting an evaluation nobody saw.
func TestStoryPointsEvalReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	err := writeStoryPointsEval(&firstWriteFails{}, EvalCounts{}, 0, nil)
	if err == nil || !strings.Contains(err.Error(), "closed pipe") {
		t.Fatalf("err = %v, want the failed write's error", err)
	}
}

// evalEvalTown writes a town of n landings, each taking ten minutes, landed one
// day apart from 2026-10-01, with an estimate for every one. The blended
// prediction is the actual ten minutes, the shape input twenty and the embed
// input five, so every figure the table prints has one known value.
func evalEvalTown(t *testing.T, n int) string {
	t.Helper()
	town := evalJoinTown(t)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	landings := make([]land.LandingRecord, 0, n)
	for i := 0; i < n; i++ {
		bead := fmt.Sprintf("gt-%02d", i)
		landings = append(landings, evalJoinLanding(bead, 600, day.AddDate(0, 0, i), false, false))
		evalJoinAppendEstimate(t, town, evalJoinEstimate(bead, 10, floatPtr(20), floatPtr(5), nil))
	}
	evalJoinWriteLandings(t, town, "gastown", landings...)
	return town
}

// TestStoryPointsEvalCommand: the registered command reads the town's ledgers
// and estimate log and prints the table over the newest three rows in ten.
func TestStoryPointsEvalCommand(t *testing.T) {
	t.Parallel()
	town := evalEvalTown(t, 100)
	var out bytes.Buffer
	c := Commands{
		Wire: func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
		Out:  &out,
	}
	if err := Run(context.Background(), CommandStoryPointsEval, c); err != nil {
		t.Fatalf("storypoints-eval: %v", err)
	}
	text := out.String()
	if want := "story points evaluation: 30 test rows of 100 joined rows (100 landing records read)"; !strings.Contains(text, want) {
		t.Errorf("out = %q, want it to hold %q", text, want)
	}
	if want := evalTableRow("blended", "30", "0.00", "0.00", "1.00", "n/a", "30", "0.00", "0.00"); !want.MatchString(text) {
		t.Errorf("out = %q, want a blended row %v", text, want)
	}
	if want := evalTableRow("shape", "30", "10.00", "10.00", "0.00", "n/a", "30", "0.69", "0.69"); !want.MatchString(text) {
		t.Errorf("out = %q, want a shape row %v", text, want)
	}
	if want := evalTableRow("jev", "0", "n/a", "n/a", "n/a", "n/a", "0", "n/a", "n/a"); !want.MatchString(text) {
		t.Errorf("out = %q, want a jev row %v: no row carries the input", text, want)
	}
}

// TestStoryPointsEvalSince: --since moves the test part to the rows landed at
// or after the day, and a day that is not a date is refused rather than read as
// the default split.
func TestStoryPointsEvalSince(t *testing.T) {
	t.Parallel()
	town := evalEvalTown(t, 40)
	var out bytes.Buffer
	c := Commands{
		Wire:             func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
		Out:              &out,
		StoryPointsSince: "2026-10-31",
	}
	if err := Run(context.Background(), CommandStoryPointsEval, c); err != nil {
		t.Fatalf("storypoints-eval: %v", err)
	}
	text := out.String()
	if want := "story points evaluation: 10 test rows of 40 joined rows"; !strings.Contains(text, want) {
		t.Errorf("out = %q, want the ten days from 2026-10-31 as the test part", text)
	}
	if !strings.Contains(text, "the test part is under 30 rows, so no error figures are printed") {
		t.Errorf("out = %q, want the short-part line", text)
	}

	t.Run("not a date", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		c := Commands{
			Wire:             func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
			Out:              &out,
			StoryPointsSince: "10/31/2026",
		}
		err := Run(context.Background(), CommandStoryPointsEval, c)
		if err == nil || !strings.Contains(err.Error(), "--since") || !strings.Contains(err.Error(), "10/31/2026") {
			t.Fatalf("err = %v, want it to refuse the --since value", err)
		}
		if out.Len() != 0 {
			t.Errorf("out = %q, want nothing printed for a refused --since", out.String())
		}
	})
}

// TestStoryPointsEvalJoinError: a ledger the join cannot read fails the command
// with the path named, rather than printing an empty evaluation as a town with
// nothing to measure.
func TestStoryPointsEvalJoinError(t *testing.T) {
	t.Parallel()
	town := evalJoinTown(t)
	path := filepath.Join(town, ".runtime", "landings", "gastown.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("make the ledger path: %v", err)
	}
	var out bytes.Buffer
	c := Commands{
		Wire: func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
		Out:  &out,
	}
	err := Run(context.Background(), CommandStoryPointsEval, c)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want it to name %s", err, path)
	}
}

package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// The --json object read as an outside caller reads it: the JSON field names are
// the contract, so the test declares them rather than reusing the writer's own
// types. A figure is a json.Number, read as the literal the object holds, so
// the test compares the object with the text cell figure for figure; an absent
// figure is the empty json.Number (the object's null).
type evalJSONReadPred struct {
	Name     string      `json:"name"`
	N        int         `json:"n"`
	MAE      json.Number `json:"mae"`
	MedianAE json.Number `json:"median_ae"`
	Within   json.Number `json:"within_1_5x"`
	Rho      json.Number `json:"rank_rho"`
	LogN     int         `json:"log_n"`
	LogRMS   json.Number `json:"log_rms"`
	LogBias  json.Number `json:"log_bias"`
}

// evalJSONReadTable is one table of predictor rows: the base table, or the
// capped table which also names its cap.
type evalJSONReadTable struct {
	CapMinutes int                `json:"cap_minutes"`
	Predictors []evalJSONReadPred `json:"predictors"`
}

// evalJSONReadSplit is one group of a split.
type evalJSONReadSplit struct {
	Name       string             `json:"name"`
	Rows       int                `json:"rows"`
	Predictors []evalJSONReadPred `json:"predictors"`
}

// evalJSONRead is the whole object.
type evalJSONRead struct {
	TestRows int                 `json:"test_rows"`
	Counts   map[string]int      `json:"counts"`
	Base     *evalJSONReadTable  `json:"base"`
	Capped   *evalJSONReadTable  `json:"capped"`
	ByRounds []evalJSONReadSplit `json:"by_review_rounds"`
	BySeat   []evalJSONReadSplit `json:"by_seat_class"`
}

// evalJSONCell is one figure as the text table prints it: the literal the object
// holds, or n/a where the object holds null.
func evalJSONCell(n json.Number) string {
	if n == "" {
		return "n/a"
	}
	return string(n)
}

// evalJSONTextRow matches one predictor row of the text table, whatever column
// widths tabwriter chose, and captures its nine cells in the header's order.
var evalJSONTextRow = regexp.MustCompile(`(?m)^\s*(blended|shape|embed|jev|constant)\s+` +
	`(\d+)\s+(n/a|-?\d+\.\d{2})\s+(n/a|-?\d+\.\d{2})\s+(n/a|-?\d+\.\d{2})\s+` +
	`(n/a|-?\d+\.\d{2})\s+(\d+)\s+(n/a|-?\d+\.\d{2})\s+(n/a|-?\d+\.\d{2})$`)

// evalJSONRowPattern is the text row an object's predictor entry says the table
// prints: the same cells, an absent figure as n/a. indented matches a row under
// a split's group heading.
func evalJSONRowPattern(p evalJSONReadPred, indented bool) *regexp.Regexp {
	prefix := "^"
	if indented {
		prefix = `^\s*`
	}
	cells := []string{
		strconv.Itoa(p.N),
		evalJSONCell(p.MAE), evalJSONCell(p.MedianAE), evalJSONCell(p.Within),
		evalJSONCell(p.Rho), strconv.Itoa(p.LogN),
		evalJSONCell(p.LogRMS), evalJSONCell(p.LogBias),
	}
	return regexp.MustCompile(`(?m)` + prefix + regexp.QuoteMeta(p.Name) + `\s+` + strings.Join(cells, `\s+`) + `$`)
}

// checkEvalJSONTable holds one table in the object to the text section it is
// printed as: the same number of predictor rows, and each figure the cell the
// text prints.
func checkEvalJSONTable(t *testing.T, where, text string, preds []evalJSONReadPred, indented bool) {
	t.Helper()
	if len(preds) == 0 {
		t.Fatalf("%s: the JSON carries no predictor row", where)
	}
	if got := len(evalJSONTextRow.FindAllStringSubmatch(text, -1)); got != len(preds) {
		t.Errorf("%s: the text prints %d predictor rows, the JSON carries %d", where, got, len(preds))
	}
	for _, p := range preds {
		if want := evalJSONRowPattern(p, indented); !want.MatchString(text) {
			t.Errorf("%s: the text lacks the row the JSON prints as %v", where, want)
		}
	}
}

// evalJSONTextGroup is one group a split section prints: its name, the row count
// from its heading, and the lines under it.
type evalJSONTextGroup struct {
	name string
	rows int
	text string
}

// evalJSONGroupHeading matches a split section's group heading, "  1 round: 15
// test rows", and captures the name and the count.
var evalJSONGroupHeading = regexp.MustCompile(`(?m)^\s+(.+): (\d+) test rows?$`)

// evalJSONSplitGroups reads a split section as its groups, in the text's order. A
// group's text runs to the next group's heading.
func evalJSONSplitGroups(t *testing.T, section string) []evalJSONTextGroup {
	t.Helper()
	matches := evalJSONGroupHeading.FindAllStringSubmatchIndex(section, -1)
	groups := make([]evalJSONTextGroup, 0, len(matches))
	for i, m := range matches {
		end := len(section)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		name := section[m[2]:m[3]]
		rows, err := strconv.Atoi(section[m[4]:m[5]])
		if err != nil {
			t.Fatalf("group %q: %v", name, err)
		}
		groups = append(groups, evalJSONTextGroup{name: name, rows: rows, text: section[m[0]:end]})
	}
	return groups
}

// checkEvalJSONSplits holds one split in the object to its text section: the same
// groups in the same order with the same counts, and under each group the same
// figures — none for a group the text prints as a count alone.
func checkEvalJSONSplits(t *testing.T, where, text string, splits []evalJSONReadSplit) {
	t.Helper()
	groups := evalJSONSplitGroups(t, text)
	if len(groups) != len(splits) {
		t.Fatalf("%s: the text prints %d groups, the JSON carries %d", where, len(groups), len(splits))
	}
	for i, g := range groups {
		s := splits[i]
		if s.Name != g.name || s.Rows != g.rows {
			t.Errorf("%s group %d: JSON is %q with %d rows, text is %q with %d rows", where, i, s.Name, s.Rows, g.name, g.rows)
			continue
		}
		if s.Predictors == nil {
			if want := "under 5 test rows, so no figures"; !strings.Contains(g.text, want) {
				t.Errorf("%s group %q: the JSON carries no figures and the text lacks %q", where, g.name, want)
			}
			continue
		}
		checkEvalJSONTable(t, where+" group "+g.name, g.text, s.Predictors, true)
	}
}

// evalJSONTown writes a hundred beads, one landed a day from 2026-10-01, whose
// newest thirty — the test part — spread over every review-round and seat-class
// group a split prints and over a first attempt long enough for the cap to move
// a figure. The bead at index 70 took the 103 hours of gt-8r17fp, every other
// took ten minutes. Every estimate carries a twenty-minute shape input, so the
// shape row prints figures while the embed and jev rows print none.
func evalJSONTown(t *testing.T) string {
	t.Helper()
	town := evalJoinTown(t)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	landings := make([]land.LandingRecord, 0, 100)
	for i := 0; i < 100; i++ {
		bead := fmt.Sprintf("gt-%02d", i)
		secs := 600.0
		spec := evalSplitsRow{rounds: 1, agent: "sbx-haiku", blended: 10}
		switch {
		case i == 70:
			secs, spec.blended = 6204*60, 12
		case i > 70 && i < 85:
			spec.blended = 12
		case i == 85:
			spec = evalSplitsRow{blended: 10}
		case i < 96:
			spec = evalSplitsRow{rounds: 2, agent: "deepseek-v3", blended: 5}
		case i > 85:
			spec = evalSplitsRow{rounds: 3, agent: "sbx-haiku", blended: 10}
		}
		rec := evalJoinLanding(bead, secs, day.AddDate(0, 0, i), false, false)
		rec.OMRounds, rec.Agent = spec.rounds, spec.agent
		landings = append(landings, rec)
		evalJoinAppendEstimate(t, town, evalJoinEstimate(bead, spec.blended, floatPtr(20), nil, nil))
	}
	evalJoinWriteLandings(t, town, "gastown", landings...)
	return town
}

// evalJSONRun runs storypoints-eval over town and returns what it printed, asking
// for the JSON object when asJSON is set.
func evalJSONRun(t *testing.T, town string, asJSON bool) string {
	t.Helper()
	var out bytes.Buffer
	c := Commands{
		Wire:            func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
		Out:             &out,
		StoryPointsJSON: asJSON,
	}
	if err := Run(context.Background(), CommandStoryPointsEval, c); err != nil {
		t.Fatalf("storypoints-eval (json=%v): %v", asJSON, err)
	}
	return out.String()
}

// TestStoryPointsEvalJSONTables: --json prints one JSON object carrying the four
// tables the text prints — the base, capped, review-round and seat-class tables
// — and every figure in it is the cell the text table prints, with null where
// the text prints n/a.
//
// The fixture's base blended MAE is (6192 + 14*2 + 10*5) / 30 = 209.00: the
// 103-hour attempt counts at its 6204 minutes. Capped at 90 it counts as 78, so
// the capped MAE is (78 + 28 + 50) / 30 = 5.20. Pinning both shows the object
// carries the two tables and not one of them twice.
func TestStoryPointsEvalJSONTables(t *testing.T) {
	t.Parallel()
	town := evalJSONTown(t)
	text := evalJSONRun(t, town, false)
	raw := evalJSONRun(t, town, true)
	var doc evalJSONRead
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("--json did not print one JSON object: %v\n%s", err, raw)
	}
	if doc.TestRows != 30 {
		t.Errorf("test_rows = %d, want the thirty-row test part", doc.TestRows)
	}
	if doc.Counts["joined"] != 100 || doc.Counts["records"] != 100 {
		t.Errorf("counts = %v, want the 100 joined rows of 100 landing records the text names", doc.Counts)
	}
	for _, key := range []string{"no_estimate", "lower_bound", "upper_bound", "absent", "malformed_log"} {
		if _, ok := doc.Counts[key]; !ok {
			t.Errorf("counts lacks %q", key)
		}
	}

	baseText, rest, ok := strings.Cut(text, cappedHeading)
	if !ok {
		t.Fatalf("out = %q, want the capped table headed %q", text, cappedHeading)
	}
	cappedText, splitsText, ok := strings.Cut(rest, "by review rounds (the test part)")
	if !ok {
		t.Fatalf("out = %q, want the review-round split", text)
	}
	roundsText, seatText, ok := strings.Cut(splitsText, "by seat class (the test part)")
	if !ok {
		t.Fatalf("out = %q, want the seat-class split", text)
	}

	if doc.Base == nil {
		t.Fatal("--json carries no base table")
	}
	checkEvalJSONTable(t, "the base table", baseText, doc.Base.Predictors, false)
	if got := evalJSONCell(doc.Base.Predictors[0].MAE); got != "209.00" {
		t.Errorf("base blended MAE = %q, want the 6270/30 the text prints as 209.00", got)
	}

	if doc.Capped == nil {
		t.Fatal("--json carries no capped table")
	}
	if doc.Capped.CapMinutes != storyPointsEvalCapMinutes {
		t.Errorf("capped cap_minutes = %d, want %d", doc.Capped.CapMinutes, storyPointsEvalCapMinutes)
	}
	checkEvalJSONTable(t, "the capped table", cappedText, doc.Capped.Predictors, false)
	if got := evalJSONCell(doc.Capped.Predictors[0].MAE); got != "5.20" {
		t.Errorf("capped blended MAE = %q, want the 156/30 the text prints as 5.20", got)
	}

	checkEvalJSONSplits(t, "by review rounds", roundsText, doc.ByRounds)
	checkEvalJSONSplits(t, "by seat class", seatText, doc.BySeat)
}

// TestStoryPointsEvalJSONShortPart: a test part under storyPointsEvalMinTest
// rows carries the counts and the row count and no table, as the text prints
// the counts and no figures.
func TestStoryPointsEvalJSONShortPart(t *testing.T) {
	t.Parallel()
	// Forty landings split thirty-seventy: the newest twelve are the test part.
	town := evalEvalTown(t, 40)
	raw := evalJSONRun(t, town, true)
	var doc evalJSONRead
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("--json did not print one JSON object: %v\n%s", err, raw)
	}
	if doc.TestRows != 12 {
		t.Errorf("test_rows = %d, want the twelve-row test part", doc.TestRows)
	}
	if doc.Counts["joined"] != 40 || doc.Counts["records"] != 40 {
		t.Errorf("counts = %v, want the 40 joined rows of 40 landing records", doc.Counts)
	}
	if doc.Base != nil || doc.Capped != nil || doc.ByRounds != nil || doc.BySeat != nil {
		t.Errorf("--json carries a table for a test part under %d rows", storyPointsEvalMinTest)
	}
}

// TestStoryPointsEvalJSONReportsAFailedWrite: a write the JSON object cannot
// make fails the run, rather than reporting an evaluation nobody received.
func TestStoryPointsEvalJSONReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	c := Commands{
		Wire:            func(context.Context) (RealTown, error) { return RealTown{Town: evalJSONTown(t)}, nil },
		Out:             failOnWrite{marker: "test_rows"},
		StoryPointsJSON: true,
	}
	err := Run(context.Background(), CommandStoryPointsEval, c)
	if err == nil || !strings.Contains(err.Error(), "closed pipe") {
		t.Fatalf("err = %v, want the failed write's error", err)
	}
}

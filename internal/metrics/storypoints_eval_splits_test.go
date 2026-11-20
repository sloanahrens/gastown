package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// evalSplitsRow is one landing of the splits fixture: the review rounds and the
// seat agent its record carries, and the blended estimate the bead had.
type evalSplitsRow struct {
	rounds  int
	agent   string
	blended float64
}

// evalSplitsTown writes a town of n landings, one landed day apart from
// 2026-10-01, each taking ten minutes and carrying the rounds, agent and
// blended estimate give returns for its index, so a caller can lay out the
// newest rows—the test part—by index. Every estimate carries a shape input of
// twenty minutes.
func evalSplitsTown(t *testing.T, n int, give func(i int) evalSplitsRow) string {
	t.Helper()
	town := evalJoinTown(t)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	landings := make([]land.LandingRecord, 0, n)
	for i := 0; i < n; i++ {
		bead := fmt.Sprintf("gt-%02d", i)
		rec := evalJoinLanding(bead, 600, day.AddDate(0, 0, i), false, false)
		spec := give(i)
		rec.OMRounds = spec.rounds
		rec.Agent = spec.agent
		landings = append(landings, rec)
		evalJoinAppendEstimate(t, town, evalJoinEstimate(bead, spec.blended, floatPtr(20), nil, nil))
	}
	evalJoinWriteLandings(t, town, "gastown", landings...)
	return town
}

// splitTableRow matches one row of a split's predictor table: evalTableRow with
// the indentation the table stands under its group heading.
func splitTableRow(name string, cells ...string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*` + name + `\s+` + strings.Join(cells, `\s+`) + `$`)
}

// TestStoryPointsEvalSplits: the command prints the same predictor table for
// the test rows grouped by the review rounds and by the seat class of the agent
// their landing records carry, and a group under five test rows prints its
// count alone.
//
// The town is a hundred beads, so the test part is the newest thirty: fifteen
// took one round on the haiku seat with a blended estimate two minutes over the
// ten they took, ten took two rounds on a deepseek agent at five minutes under,
// four took three rounds on the haiku seat at the estimate, and one record
// carries no rounds and no agent.
func TestStoryPointsEvalSplits(t *testing.T) {
	t.Parallel()
	const train = 70
	town := evalSplitsTown(t, 100, func(i int) evalSplitsRow {
		switch {
		case i < train:
			return evalSplitsRow{rounds: 1, agent: "sbx-haiku", blended: 10}
		case i < train+15:
			return evalSplitsRow{rounds: 1, agent: "sbx-haiku", blended: 12}
		case i == train+15:
			return evalSplitsRow{blended: 10}
		case i < train+26:
			return evalSplitsRow{rounds: 2, agent: "deepseek-v3", blended: 5}
		default:
			return evalSplitsRow{rounds: 3, agent: "sbx-haiku", blended: 10}
		}
	})
	var out bytes.Buffer
	c := Commands{
		Wire: func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
		Out:  &out,
	}
	if err := Run(context.Background(), CommandStoryPointsEval, c); err != nil {
		t.Fatalf("storypoints-eval: %v", err)
	}
	text := out.String()
	if want := "story points evaluation: 30 test rows of 100 joined rows"; !strings.Contains(text, want) {
		t.Fatalf("out = %q, want it to hold %q", text, want)
	}

	rounds := section(t, text, "by review rounds (the test part)", "by seat class (the test part)")
	for _, want := range []string{
		"  1 round: 15 test rows",
		"  2 rounds: 10 test rows",
		"  3+ rounds: 4 test rows",
		"  no rounds recorded: 1 test row",
	} {
		if !strings.Contains(rounds, want) {
			t.Errorf("by review rounds = %q, want it to hold %q", rounds, want)
		}
	}
	// One round: every estimate two minutes over the actual ten.
	if want := splitTableRow("blended", "15", "2.00", "2.00", "1.00", "n/a", "15", `.*`, `.*`); !want.MatchString(rounds) {
		t.Errorf("by review rounds = %q, want a blended row %v", rounds, want)
	}
	// Two rounds: every estimate five minutes under.
	if want := splitTableRow("blended", "10", "5.00", "5.00", "0.00", "n/a", "10", `.*`, `.*`); !want.MatchString(rounds) {
		t.Errorf("by review rounds = %q, want a blended row %v", rounds, want)
	}
	// A group under five test rows prints its count and no figures: no row
	// scores the four rows of the 3+ rounds group.
	if want := splitTableRow("blended", "4", `.*`); want.MatchString(rounds) {
		t.Errorf("by review rounds = %q, want no figures for the four-row group", rounds)
	}
	if want := "3+ rounds: 4 test rows\n    under 5 test rows, so no figures"; !strings.Contains(rounds, want) {
		t.Errorf("by review rounds = %q, want %q", rounds, want)
	}
	if want := "no rounds recorded: 1 test row\n    under 5 test rows, so no figures"; !strings.Contains(rounds, want) {
		t.Errorf("by review rounds = %q, want the unrecorded row counted apart from one round", rounds)
	}

	seats := section(t, text, "by seat class (the test part)", "")
	for _, want := range []string{
		"  haiku: 19 test rows",
		"  flash: 10 test rows",
		"  (no seat class): 1 test row",
	} {
		if !strings.Contains(seats, want) {
			t.Errorf("by seat class = %q, want it to hold %q", seats, want)
		}
	}
	// Haiku: fifteen rows two minutes over and four at the estimate, so the
	// mean absolute error is 30 / 19.
	if want := splitTableRow("blended", "19", "1.58", "2.00", "1.00", "n/a", "19", `.*`, `.*`); !want.MatchString(seats) {
		t.Errorf("by seat class = %q, want a blended row %v (the deepseek agent is the flash class)", seats, want)
	}
	// Flash: the ten two-round rows, five minutes under each.
	if want := splitTableRow("blended", "10", "5.00", "5.00", "0.00", "n/a", "10", `.*`, `.*`); !want.MatchString(seats) {
		t.Errorf("by seat class = %q, want a blended row %v", seats, want)
	}
	if want := "  (no seat class): 1 test row\n    under 5 test rows, so no figures"; !strings.Contains(seats, want) {
		t.Errorf("by seat class = %q, want the seatless row counted alone", seats)
	}
}

// section is the text after the from marker and before the to marker, or to
// the end when to is empty. A missing marker fails the test: a split the step
// did not print is the failure the caller is looking for.
func section(t *testing.T, text, from, to string) string {
	t.Helper()
	_, after, ok := strings.Cut(text, from)
	if !ok {
		t.Fatalf("out = %q, want the section %q", text, from)
	}
	if to == "" {
		return after
	}
	before, _, ok := strings.Cut(after, to)
	if !ok {
		t.Fatalf("out = %q, want the section %q after %q", text, to, from)
	}
	return before
}

// TestStoryPointsEvalSplitsShortPart: a test part under storyPointsEvalMinTest
// rows prints the base table's short-part line and no split table, since a
// group's figure would come from a part too small to read.
func TestStoryPointsEvalSplitsShortPart(t *testing.T) {
	t.Parallel()
	town := evalSplitsTown(t, 40, func(int) evalSplitsRow {
		return evalSplitsRow{rounds: 1, agent: "sbx-haiku", blended: 12}
	})
	var out bytes.Buffer
	c := Commands{
		Wire: func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
		Out:  &out,
	}
	if err := Run(context.Background(), CommandStoryPointsEval, c); err != nil {
		t.Fatalf("storypoints-eval: %v", err)
	}
	text := out.String()
	if want := "the test part is under 30 rows, so no error figures are printed"; !strings.Contains(text, want) {
		t.Errorf("out = %q, want it to hold %q", text, want)
	}
	for _, unwanted := range []string{"by review rounds", "by seat class", "predictor"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("out = %q, want no split table at %q", text, unwanted)
		}
	}
}

// failOnWrite refuses every write holding the marker and accepts the rest.
type failOnWrite struct{ marker string }

func (w failOnWrite) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.marker) {
		return 0, errors.New("closed pipe")
	}
	return len(p), nil
}

// TestStoryPointsEvalSplitsFailedWrite: a line of the evaluation that cannot be
// written fails the run rather than reporting an evaluation nobody saw, and the
// base table and the review-round split each refuse on their own write.
func TestStoryPointsEvalSplitsFailedWrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		out    io.Writer
		marker string
	}{
		{"the split alone", &firstWriteFails{}, ""},
		{"the base table", failOnWrite{marker: "story points evaluation"}, "story points evaluation"},
		{"the review-round split", failOnWrite{marker: "by review rounds"}, "by review rounds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out io.Writer = tc.out
			var err error
			if tc.marker == "" {
				err = writeEvalSplits(out, "by review rounds (the test part)",
					[]EvalSplit{{Name: "1 round", Rows: storyPointsEvalMinGroup}})
			} else {
				c := Commands{
					Wire: func(context.Context) (RealTown, error) {
						return RealTown{Town: evalSplitsTown(t, 100, func(int) evalSplitsRow {
							return evalSplitsRow{rounds: 1, agent: "sbx-haiku", blended: 12}
						})}, nil
					},
					Out: out,
				}
				err = Run(context.Background(), CommandStoryPointsEval, c)
			}
			if err == nil || !strings.Contains(err.Error(), "closed pipe") {
				t.Fatalf("err = %v, want the failed write's error", err)
			}
		})
	}
}

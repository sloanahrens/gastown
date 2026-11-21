package metrics

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// cappedHeading heads the capped table the command prints (gt-xwqe1r). The
// test names it whole so a changed cap fails to find the section.
const cappedHeading = "the same table with every polecat time capped at 90 minutes (the test part)"

// evalCappedTown writes a hundred landings, one a day from 2026-10-01, each
// estimated at ten blended minutes with a twenty-minute shape input. The bead
// at index long lands after a first attempt of 6204 minutes, the 103 hours of
// gt-8r17fp; every other takes ten.
func evalCappedTown(t *testing.T, long int) string {
	t.Helper()
	town := evalJoinTown(t)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	landings := make([]land.LandingRecord, 0, 100)
	for i := 0; i < 100; i++ {
		bead := fmt.Sprintf("gt-%02d", i)
		secs := 600.0
		if i == long {
			secs = 6204 * 60
		}
		landings = append(landings, evalJoinLanding(bead, secs, day.AddDate(0, 0, i), false, false))
		evalJoinAppendEstimate(t, town, evalJoinEstimate(bead, 10, floatPtr(20), nil, nil))
	}
	evalJoinWriteLandings(t, town, "gastown", landings...)
	return town
}

// evalCappedRun runs the command over town and returns its output.
func evalCappedRun(t *testing.T, town string) string {
	t.Helper()
	var out bytes.Buffer
	c := Commands{
		Wire: func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
		Out:  &out,
	}
	if err := Run(context.Background(), CommandStoryPointsEval, c); err != nil {
		t.Fatalf("storypoints-eval: %v", err)
	}
	return out.String()
}

// TestStoryPointsEvalCappedTable: the command prints the base table again with
// every polecat time capped at 90 minutes and says so on the table, so the one
// 103-hour attempt counts as 90 while the ten-minute attempts stay ten, and the
// base table keeps reading the uncapped times.
//
// The long attempt is the newest test row, so the base table shows it at its
// 6204 minutes: the blended mean absolute error over the thirty test rows is
// 6194/30, and the shape input's is (29*10 + 6184)/30. Capped, the same rows
// read 80/30 and (29*10 + 70)/30.
func TestStoryPointsEvalCappedTable(t *testing.T) {
	t.Parallel()
	text := evalCappedRun(t, evalCappedTown(t, 99))
	base, capped, ok := strings.Cut(text, cappedHeading)
	if !ok {
		t.Fatalf("out = %q, want the capped table headed %q", text, cappedHeading)
	}
	if !strings.Contains(capped, "MAE (min, capped at 90)") {
		t.Errorf("capped table = %q, want its error columns to name the cap", capped)
	}
	// The base table is unchanged: it still measures the 6204 minutes as landed.
	if want := evalTableRow("blended", "30", "206.47", "0.00", "0.97", "n/a", "30", `.*`, `.*`); !want.MatchString(base) {
		t.Errorf("base table = %q, want a blended row %v", base, want)
	}
	if want := evalTableRow("shape", "30", "215.80", "10.00", "0.00", "n/a", "30", `.*`, `.*`); !want.MatchString(base) {
		t.Errorf("base table = %q, want a shape row %v", base, want)
	}
	// The capped table caps the long attempt at exactly 90 minutes and leaves
	// the ten-minute attempts at ten.
	if want := evalTableRow("blended", "30", "2.67", "0.00", "0.97", "n/a", "30", `.*`, `.*`); !want.MatchString(capped) {
		t.Errorf("capped table = %q, want a blended row %v", capped, want)
	}
	if want := evalTableRow("shape", "30", "12.00", "10.00", "0.00", "n/a", "30", `.*`, `.*`); !want.MatchString(capped) {
		t.Errorf("capped table = %q, want a shape row %v", capped, want)
	}
}

// TestStoryPointsEvalCappedFailedWrite: the capped table is a section of its
// own, and a write it cannot make fails the run rather than reporting an
// evaluation nobody saw.
func TestStoryPointsEvalCappedFailedWrite(t *testing.T) {
	t.Parallel()
	c := Commands{
		Wire: func(context.Context) (RealTown, error) {
			return RealTown{Town: evalCappedTown(t, 99)}, nil
		},
		Out: failOnWrite{marker: "capped at"},
	}
	err := Run(context.Background(), CommandStoryPointsEval, c)
	if err == nil || !strings.Contains(err.Error(), "closed pipe") {
		t.Fatalf("err = %v, want the failed write's error", err)
	}
}

// TestStoryPointsEvalCappedShortPart: a test part under storyPointsEvalMinTest
// rows prints no capped table, since a capped figure would come from a part too
// small to read.
func TestStoryPointsEvalCappedShortPart(t *testing.T) {
	t.Parallel()
	text := evalCappedRun(t, evalEvalTown(t, 40))
	if strings.Contains(text, "capped at") {
		t.Errorf("out = %q, want no capped table for a short test part", text)
	}
}

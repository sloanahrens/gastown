package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// evalDeclaredRow is a fixed evaluation row whose estimate declared the given
// added lines; zero is a bead whose Size section declared none.
func evalDeclaredRow(bead string, declared, actual, blended float64, shape, embed, jev *float64) EvalRow {
	row := evalTestRow(bead, actual, blended, shape, embed, jev)
	row.DeclaredLines = declared
	return row
}

// TestStoryPointsEvalDeclaredBuckets: each row lands in the bucket its declared
// Change lines name, both edges of every bucket included, and a row that
// declared none lands in the none bucket. Each bucket's figures are the medians
// and shares of its own rows.
func TestStoryPointsEvalDeclaredBuckets(t *testing.T) {
	t.Parallel()
	rows := []EvalRow{
		evalDeclaredRow("a1", 10, 4, 6, evalMinutes(3), nil, evalMinutes(5)),
		evalDeclaredRow("a2", 50, 12, 10, evalMinutes(9), evalMinutes(8), nil),
		evalDeclaredRow("b1", 51, 22, 20, evalMinutes(20), nil, nil),
		evalDeclaredRow("b2", 100, 8, 9, nil, nil, evalMinutes(10)),
		evalDeclaredRow("c1", 101, 30, 25, nil, nil, nil),
		evalDeclaredRow("c2", 200, 14, 12, nil, nil, nil),
		evalDeclaredRow("d1", 201, 60, 50, nil, nil, nil),
		evalDeclaredRow("d2", 500, 90, 40, nil, nil, nil),
		evalDeclaredRow("e1", 0, 5, 5, nil, nil, nil),
		evalDeclaredRow("e2", 0, 7, 7, nil, nil, evalMinutes(4)),
	}
	buckets := evalDeclaredBuckets(rows)
	if len(buckets) != len(evalDeclaredLabels) {
		t.Fatalf("evalDeclaredBuckets returned %d buckets, want %d", len(buckets), len(evalDeclaredLabels))
	}
	// Hand-computed: the median of an even count averages the two middle values,
	// and a share is over the bucket's own count.
	want := []struct {
		label                     string
		count                     int
		actual, shape, embed, jev *float64
		blended                   *float64
		overTenHalf, overTwenty   float64
	}{
		{"up to 50", 2, floatPtr(8), floatPtr(6), floatPtr(8), floatPtr(5), floatPtr(8), 0.5, 0},
		{"51 to 100", 2, floatPtr(15), floatPtr(20), nil, floatPtr(10), floatPtr(14.5), 0.5, 0.5},
		{"101 to 200", 2, floatPtr(22), nil, nil, nil, floatPtr(18.5), 1, 0.5},
		{"over 200", 2, floatPtr(75), nil, nil, nil, floatPtr(45), 1, 1},
		{"none declared", 2, floatPtr(6), nil, nil, floatPtr(4), floatPtr(6), 0, 0},
	}
	for i, w := range want {
		got := buckets[i]
		if got.Label != w.label {
			t.Fatalf("bucket %d is %q, want %q", i, got.Label, w.label)
		}
		if got.Count != w.count {
			t.Errorf("%s: count = %d, want %d", w.label, got.Count, w.count)
		}
		checkEvalJoinFloat(t, w.label+" actual median", got.ActualMedian, w.actual)
		checkEvalJoinFloat(t, w.label+" shape median", got.ShapeMedian, w.shape)
		checkEvalJoinFloat(t, w.label+" embed median", got.EmbedMedian, w.embed)
		checkEvalJoinFloat(t, w.label+" jev median", got.JevMedian, w.jev)
		checkEvalJoinFloat(t, w.label+" blended median", got.BlendedMedian, w.blended)
		if got.OverTenHalfShare != w.overTenHalf {
			t.Errorf("%s: share over 10.5 = %v, want %v", w.label, got.OverTenHalfShare, w.overTenHalf)
		}
		if got.OverTwentyShare != w.overTwenty {
			t.Errorf("%s: share over 20 = %v, want %v", w.label, got.OverTwentyShare, w.overTwenty)
		}
	}
}

// TestStoryPointsEvalDeclaredJoin: the join reads the declared lines of the
// latest estimate line the log holds for a bead, and a bead whose line has no
// declared lines joins as zero, the none bucket.
func TestStoryPointsEvalDeclaredJoin(t *testing.T) {
	t.Parallel()
	town := evalJoinTown(t)
	evalJoinWriteLandings(t, town, "gastown",
		evalJoinLanding("gt-a", 600, evalJoinAt(1), false, false),
		evalJoinLanding("gt-b", 600, evalJoinAt(2), false, false),
	)
	first := evalJoinEstimate("gt-a", 10, floatPtr(10), nil, nil)
	first.DeclaredLines = 40
	evalJoinAppendEstimate(t, town, first)
	second := evalJoinEstimate("gt-a", 12, floatPtr(10), nil, nil)
	second.DeclaredLines = 80
	evalJoinAppendEstimate(t, town, second)
	evalJoinAppendEstimate(t, town, evalJoinEstimate("gt-b", 10, floatPtr(10), nil, nil))

	rows, _, err := joinEval(town)
	if err != nil {
		t.Fatalf("joinEval: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("joinEval returned %d rows, want 2", len(rows))
	}
	wantDeclared := map[string]float64{"gt-a": 80, "gt-b": 0}
	for _, row := range rows {
		if got := row.DeclaredLines; got != wantDeclared[row.Bead] {
			t.Errorf("%s: DeclaredLines = %v, want %v", row.Bead, got, wantDeclared[row.Bead])
		}
	}
}

// evalDeclaredTown writes n beads, one landed a day from 2026-10-01, that
// declare lines by their index: none, then 30, 75, 150 and 300. Every bead takes
// ten minutes except those declaring 300, which take thirty, so each bucket's
// median and shares differ. Every estimate blends to ten minutes with a
// twenty-minute shape input.
func evalDeclaredTown(t *testing.T, n int) string {
	t.Helper()
	town := evalJoinTown(t)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	declared := []float64{0, 30, 75, 150, 300}
	landings := make([]land.LandingRecord, 0, n)
	for i := 0; i < n; i++ {
		bead := fmt.Sprintf("gt-%03d", i)
		lines := declared[i%len(declared)]
		secs := 600.0
		if lines == 300 {
			secs = 1800
		}
		landings = append(landings, evalJoinLanding(bead, secs, day.AddDate(0, 0, i), false, false))
		est := evalJoinEstimate(bead, 10, floatPtr(20), nil, nil)
		est.DeclaredLines = lines
		evalJoinAppendEstimate(t, town, est)
	}
	evalJoinWriteLandings(t, town, "gastown", landings...)
	return town
}

// TestStoryPointsEvalDeclaredPrinted: the text prints the declared table after
// the estimate bands, one row per bucket in order, and the --json object carries
// the same rows with the same cells. The fixture's test part is the newest
// thirty beads, six of each declared-lines group.
func TestStoryPointsEvalDeclaredPrinted(t *testing.T) {
	t.Parallel()
	town := evalDeclaredTown(t, 100)
	want := [][]string{
		{"up to 50", "6", "10.00", "20.00", "n/a", "n/a", "10.00", "0.00", "0.00"},
		{"51 to 100", "6", "10.00", "20.00", "n/a", "n/a", "10.00", "0.00", "0.00"},
		{"101 to 200", "6", "10.00", "20.00", "n/a", "n/a", "10.00", "0.00", "0.00"},
		{"over 200", "6", "30.00", "20.00", "n/a", "n/a", "10.00", "1.00", "1.00"},
		{"none declared", "6", "10.00", "20.00", "n/a", "n/a", "10.00", "0.00", "0.00"},
	}

	t.Run("printed", func(t *testing.T) {
		t.Parallel()
		text := evalJSONRun(t, town, false)
		_, rest, ok := strings.Cut(text, "\n"+evalDeclaredHeading+"\n")
		if !ok {
			t.Fatalf("out = %q, want the declared section headed %q", text, evalDeclaredHeading)
		}
		before := text[:strings.Index(text, evalDeclaredHeading)]
		for _, heading := range []string{evalBandHeading, cappedHeading} {
			if !strings.Contains(before, heading) {
				t.Errorf("the declared section does not come after %q", heading)
			}
		}
		// The declared table is printed last, so the section runs to the end.
		section := rest
		lines := strings.Split(strings.TrimSpace(section), "\n")
		if len(lines) != len(want)+1 {
			t.Fatalf("declared section has %d lines, want the header and %d buckets:\n%s", len(lines), len(want), section)
		}
		for _, row := range want {
			cells := row[1:]
			if !evalTableRow(regexp.QuoteMeta(row[0]), cells...).MatchString(section) {
				t.Errorf("declared section lacks the row %q with cells %v:\n%s", row[0], cells, section)
			}
		}
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		raw := evalJSONRun(t, town, true)
		var doc struct {
			ByDeclared []evalBandRead `json:"by_declared_lines"`
		}
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("--json did not print one JSON object: %v\n%s", err, raw)
		}
		if len(doc.ByDeclared) != len(want) {
			t.Fatalf("--json carries %d declared buckets, want %d", len(doc.ByDeclared), len(want))
		}
		for i, b := range doc.ByDeclared {
			got := []string{
				b.Label, strconv.Itoa(b.Count),
				evalJSONCell(b.ActualMedian), evalJSONCell(b.ShapeMedian),
				evalJSONCell(b.EmbedMedian), evalJSONCell(b.JevMedian),
				evalJSONCell(b.BlendedMedian),
				evalJSONCell(b.OverTenHalfShare), evalJSONCell(b.OverTwentyShare),
			}
			if !slices.Equal(got, want[i]) {
				t.Errorf("declared bucket %d: --json reads %v, the text row is %v", i, got, want[i])
			}
		}
	})

	t.Run("failed write", func(t *testing.T) {
		t.Parallel()
		c := Commands{
			Wire: func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
			Out:  failOnWrite{marker: evalDeclaredHeading},
		}
		err := Run(context.Background(), CommandStoryPointsEval, c)
		if err == nil || !strings.Contains(err.Error(), "closed pipe") {
			t.Fatalf("err = %v, want the failed write of the declared section", err)
		}
	})
}

// TestStoryPointsEvalDeclaredSeatSectionFailedWrite: a refused write of the
// seat-class split ends the run with that error, so the declared table, which
// prints after it, is not reached.
func TestStoryPointsEvalDeclaredSeatSectionFailedWrite(t *testing.T) {
	t.Parallel()
	town := evalDeclaredTown(t, 100)
	c := Commands{
		Wire: func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
		Out:  failOnWrite{marker: "by seat class"},
	}
	err := Run(context.Background(), CommandStoryPointsEval, c)
	if err == nil || !strings.Contains(err.Error(), "closed pipe") {
		t.Fatalf("err = %v, want the failed write of the seat-class split", err)
	}
}

// TestStoryPointsEvalDeclaredShortPart: a test part under storyPointsEvalMinTest
// rows prints no declared table, and the --json object carries it as null, as
// it does the bands.
func TestStoryPointsEvalDeclaredShortPart(t *testing.T) {
	t.Parallel()
	short := evalDeclaredTown(t, 20)
	if text := evalJSONRun(t, short, false); strings.Contains(text, evalDeclaredHeading) {
		t.Errorf("out = %q, want no declared section for a test part under %d rows", text, storyPointsEvalMinTest)
	}
	var doc struct {
		ByDeclared json.RawMessage `json:"by_declared_lines"`
	}
	if err := json.Unmarshal([]byte(evalJSONRun(t, short, true)), &doc); err != nil {
		t.Fatalf("--json did not print one JSON object: %v", err)
	}
	if string(doc.ByDeclared) != "null" {
		t.Errorf("by_declared_lines = %s, want null for a short test part", doc.ByDeclared)
	}
}

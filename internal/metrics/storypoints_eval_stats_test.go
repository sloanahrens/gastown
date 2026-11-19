package metrics

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"
)

// evalTestRow is one hand-built row: the actual minutes, the blended estimate
// and the three inputs, any of which may be absent.
func evalTestRow(bead string, actual, blended float64, shape, embed, jev *float64) EvalRow {
	return EvalRow{
		Bead:           bead,
		ActualMinutes:  actual,
		BlendedMinutes: blended,
		ShapeMinutes:   shape,
		EmbedMinutes:   embed,
		JevMinutes:     jev,
	}
}

func evalMinutes(v float64) *float64 {
	return &v
}

// evalStatsNamed is the entry evalStats returned for one predictor.
func evalStatsNamed(t *testing.T, stats []PredictorStats, name string) PredictorStats {
	t.Helper()
	for _, s := range stats {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("evalStats returned no %q entry", name)
	return PredictorStats{}
}

// evalStatsNear fails when an entry's figure differs from the hand-computed
// one.
func evalStatsNear(t *testing.T, entry PredictorStats, field string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s %s = %v, want %v", entry.Name, field, got, want)
	}
}

// TestStoryPointsEvalSplit: the test part is the newest 30 percent of the rows
// by landed time, or, with a since, the rows landed at or after it.
func TestStoryPointsEvalSplit(t *testing.T) {
	t.Parallel()
	at := func(hour int) time.Time {
		return time.Date(2026, 11, 1, hour, 0, 0, 0, time.UTC)
	}
	rows := func(hours ...int) []EvalRow {
		out := make([]EvalRow, len(hours))
		for i, hour := range hours {
			out[i] = EvalRow{Bead: fmt.Sprintf("b%d", hour), LandedAt: at(hour)}
		}
		return out
	}
	landedHours := func(rows []EvalRow) []int {
		out := make([]int, len(rows))
		for i, row := range rows {
			out[i] = row.LandedAt.Hour()
		}
		return out
	}
	tests := []struct {
		name      string
		rows      []EvalRow
		since     time.Time
		wantTrain []int
		wantTest  []int
	}{
		{
			name:      "the newest 30 percent is the test part",
			rows:      rows(0, 1, 2, 3, 4, 5, 6, 7, 8, 9),
			wantTrain: []int{0, 1, 2, 3, 4, 5, 6},
			wantTest:  []int{7, 8, 9},
		},
		{
			name:      "a half row rounds up",
			rows:      rows(0, 1, 2, 3, 4),
			wantTrain: []int{0, 1, 2},
			wantTest:  []int{3, 4},
		},
		{
			name:      "two rows hold one test row",
			rows:      rows(0, 1),
			wantTrain: []int{0},
			wantTest:  []int{1},
		},
		{
			name:      "one row has no test part",
			rows:      rows(0),
			wantTrain: []int{0},
			wantTest:  nil,
		},
		{
			name:      "no rows",
			wantTrain: nil,
			wantTest:  nil,
		},
		{
			name:      "the rows are split by landed time, not by input order",
			rows:      rows(4, 1, 3, 0, 2),
			wantTrain: []int{0, 1, 2},
			wantTest:  []int{3, 4},
		},
		{
			name:      "a since sends the rows landed on or after it to the test part",
			rows:      rows(0, 1, 2, 3, 4),
			since:     at(2),
			wantTrain: []int{0, 1},
			wantTest:  []int{2, 3, 4},
		},
		{
			name:      "a since after every row leaves no test row",
			rows:      rows(0, 1, 2),
			since:     at(3),
			wantTrain: []int{0, 1, 2},
			wantTest:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := landedHours(tc.rows)
			train, test := splitEval(tc.rows, tc.since)
			if got := landedHours(train); !slices.Equal(got, tc.wantTrain) {
				t.Errorf("train hours = %v, want %v", got, tc.wantTrain)
			}
			if got := landedHours(test); !slices.Equal(got, tc.wantTest) {
				t.Errorf("test hours = %v, want %v", got, tc.wantTest)
			}
			if got := landedHours(tc.rows); !slices.Equal(got, input) {
				t.Errorf("splitEval reordered the caller's rows: %v became %v", input, got)
			}
		})
	}
}

// TestStoryPointsEvalStats: a hand-built set gives the exact figures for the
// blended estimate, each input and the constant. The training median is 20
// (not the training mean of 43.3), and every predictor's error is known from
// the rows.
func TestStoryPointsEvalStats(t *testing.T) {
	t.Parallel()
	ln2 := math.Ln2
	train := []EvalRow{
		evalTestRow("train-a", 10, 0, nil, nil, nil),
		evalTestRow("train-b", 20, 0, nil, nil, nil),
		evalTestRow("train-c", 100, 0, nil, nil, nil),
	}
	test := []EvalRow{
		evalTestRow("a", 10, 10, evalMinutes(20), evalMinutes(10), evalMinutes(10)),
		evalTestRow("b", 20, 20, evalMinutes(10), nil, evalMinutes(0)),
		evalTestRow("c", 40, 40, evalMinutes(80), evalMinutes(40), evalMinutes(40)),
	}
	tests := []struct {
		name     string
		want     PredictorStats
		noSpread bool
	}{
		{
			name: "blended",
			want: PredictorStats{Name: "blended", N: 3, Within: 1, WithinOK: true, Rho: 1},
		},
		{
			// Errors 10, 10 and 40: mean 20, median 10. Ratios 2, 1/2 and 2:
			// bias ln2/3 and root mean square ln2.
			name: "shape",
			want: PredictorStats{
				Name: "shape", N: 3, MAE: 20, MedianAE: 10,
				LogRMS: ln2, LogBias: ln2 / 3,
				Within: 0, WithinOK: true, Rho: 0.5,
			},
		},
		{
			// The middle row carries no embed input, so it is not scored at all.
			name: "embed",
			want: PredictorStats{Name: "embed", N: 2, Within: 1, WithinOK: true, Rho: 1},
		},
		{
			// Errors 0, 20 and 0. The zero prediction is left out of the log
			// figures and counted, and stays in the within share as a miss.
			name: "jev",
			want: PredictorStats{
				Name: "jev", N: 3, SkippedLog: 1, MAE: 20.0 / 3,
				Within: 2.0 / 3, WithinOK: true, Rho: 0.5,
			},
		},
		{
			// Errors 10, 0 and 20 against the constant 20. Ratios 2, 1 and 1/2
			// are symmetric, so the bias is 0.
			name: "constant",
			want: PredictorStats{
				Name: "constant", N: 3, MAE: 10, MedianAE: 10,
				LogRMS: ln2 * math.Sqrt(2.0/3.0),
				Within: 1.0 / 3, WithinOK: true,
			},
			noSpread: true,
		},
	}
	stats := evalStats(train, test)
	if len(stats) != len(tests) {
		t.Fatalf("evalStats returned %d entries, want %d", len(stats), len(tests))
	}
	for i, tc := range tests {
		s := stats[i]
		if s.Name != tc.want.Name {
			t.Errorf("entry %d is %q, want %q", i, s.Name, tc.want.Name)
			continue
		}
		if s.N != tc.want.N {
			t.Errorf("%s N = %d, want %d", s.Name, s.N, tc.want.N)
		}
		if s.SkippedLog != tc.want.SkippedLog {
			t.Errorf("%s SkippedLog = %d, want %d", s.Name, s.SkippedLog, tc.want.SkippedLog)
		}
		evalStatsNear(t, s, "MAE", s.MAE, tc.want.MAE)
		evalStatsNear(t, s, "MedianAE", s.MedianAE, tc.want.MedianAE)
		evalStatsNear(t, s, "LogRMS", s.LogRMS, tc.want.LogRMS)
		evalStatsNear(t, s, "LogBias", s.LogBias, tc.want.LogBias)
		evalStatsNear(t, s, "Within", s.Within, tc.want.Within)
		evalStatsNear(t, s, "Rho", s.Rho, tc.want.Rho)
		if s.WithinOK != tc.want.WithinOK {
			t.Errorf("%s WithinOK = %v, want %v", s.Name, s.WithinOK, tc.want.WithinOK)
		}
		switch {
		case tc.noSpread && !errors.Is(s.RhoErr, ErrNoSpread):
			t.Errorf("%s RhoErr = %v, want ErrNoSpread: the constant has no spread", s.Name, s.RhoErr)
		case !tc.noSpread && s.RhoErr != nil:
			t.Errorf("%s RhoErr = %v, want none", s.Name, s.RhoErr)
		}
	}
}

// TestStoryPointsEvalStatsSkipsNonPositive: a row with a non-positive actual or
// prediction is counted and left out of the log figures, while a non-positive
// prediction still counts against the within share.
func TestStoryPointsEvalStatsSkipsNonPositive(t *testing.T) {
	t.Parallel()
	train := []EvalRow{evalTestRow("train", 10, 0, nil, nil, nil)}
	test := []EvalRow{
		evalTestRow("ratio-two", 10, 20, nil, nil, nil),
		evalTestRow("predicted-zero", 20, 0, nil, nil, nil),
		evalTestRow("actual-zero", 0, 30, nil, nil, nil),
		evalTestRow("predicted-negative", 40, -5, nil, nil, nil),
		evalTestRow("ratio-four-fifths", 50, 40, nil, nil, nil),
	}
	blended := evalStatsNamed(t, evalStats(train, test), predictorBlended)
	if blended.N != 5 {
		t.Errorf("N = %d, want 5", blended.N)
	}
	if blended.SkippedLog != 3 {
		t.Errorf("SkippedLog = %d, want 3: the zero and negative predictions and the zero actual", blended.SkippedLog)
	}
	// Errors 10, 20, 30, 45 and 10 average 23; the middle error is 20.
	evalStatsNear(t, blended, "MAE", blended.MAE, 23)
	evalStatsNear(t, blended, "MedianAE", blended.MedianAE, 20)
	// Only ln 2 and ln 0.8 enter the log figures.
	lr := math.Log(0.8)
	evalStatsNear(t, blended, "LogBias", blended.LogBias, (math.Ln2+lr)/2)
	evalStatsNear(t, blended, "LogRMS", blended.LogRMS, math.Sqrt((math.Ln2*math.Ln2+lr*lr)/2))
	// Four rows have a positive actual: 2 is outside the band, zero and -5
	// are misses, and 0.8 is inside it.
	if !blended.WithinOK {
		t.Fatal("WithinOK = false, want true")
	}
	evalStatsNear(t, blended, "Within", blended.Within, 0.25)
	if blended.RhoErr != nil {
		t.Errorf("RhoErr = %v, want none", blended.RhoErr)
	}
	evalStatsNear(t, blended, "Rho", blended.Rho, 0)
}

// TestStoryPointsEvalStatsFactorBand: a prediction exactly 1.5 times the actual
// is inside the band and one a hundredth further out is not.
func TestStoryPointsEvalStatsFactorBand(t *testing.T) {
	t.Parallel()
	train := []EvalRow{evalTestRow("train", 10, 0, nil, nil, nil)}
	test := []EvalRow{
		evalTestRow("one-and-a-half", 2, 3, nil, nil, nil),
		evalTestRow("just-inside", 100, 149, nil, nil, nil),
		evalTestRow("just-outside", 100, 151, nil, nil, nil),
		evalTestRow("three-quarters", 100, 75, nil, nil, nil),
		evalTestRow("half", 100, 50, nil, nil, nil),
	}
	blended := evalStatsNamed(t, evalStats(train, test), predictorBlended)
	if !blended.WithinOK {
		t.Fatal("WithinOK = false, want true")
	}
	evalStatsNear(t, blended, "Within", blended.Within, 3.0/5)
}

// TestStoryPointsEvalStatsConstantMedian: an even training part has a median
// between its two middle rows, and no training row leaves the constant with no
// value and no figure.
func TestStoryPointsEvalStatsConstantMedian(t *testing.T) {
	t.Parallel()
	train := []EvalRow{
		evalTestRow("train-a", 10, 0, nil, nil, nil),
		evalTestRow("train-b", 40, 0, nil, nil, nil),
	}
	test := []EvalRow{evalTestRow("a", 50, 50, nil, nil, nil)}
	constant := evalStatsNamed(t, evalStats(train, test), predictorConstant)
	if constant.N != 1 {
		t.Errorf("N = %d, want 1", constant.N)
	}
	// The median of 10 and 40 is 25, so the constant is 25 minutes off the
	// actual 50.
	evalStatsNear(t, constant, "MAE", constant.MAE, 25)

	none := evalStatsNamed(t, evalStats(nil, test), predictorConstant)
	if none.N != 0 {
		t.Errorf("with no training rows N = %d, want 0: the constant has no value", none.N)
	}
	if none.WithinOK {
		t.Error("with no training rows WithinOK = true, want false")
	}
	if !errors.Is(none.RhoErr, errEvalNoRows) {
		t.Errorf("with no training rows RhoErr = %v, want errEvalNoRows", none.RhoErr)
	}
}

// TestStoryPointsEvalStatsNoInput: a test part whose rows carry no input leaves
// that input's entry with no row scored, not with an error of zero.
func TestStoryPointsEvalStatsNoInput(t *testing.T) {
	t.Parallel()
	train := []EvalRow{evalTestRow("train", 10, 0, nil, nil, nil)}
	test := []EvalRow{evalTestRow("a", 50, 50, nil, nil, nil)}
	for _, name := range []string{predictorShape, predictorEmbed, predictorJev} {
		s := evalStatsNamed(t, evalStats(train, test), name)
		if s.N != 0 {
			t.Errorf("%s N = %d, want 0", name, s.N)
		}
		if s.MAE != 0 || s.WithinOK {
			t.Errorf("%s MAE = %v and WithinOK = %v, want an undefined entry", name, s.MAE, s.WithinOK)
		}
		if !errors.Is(s.RhoErr, errEvalNoRows) {
			t.Errorf("%s RhoErr = %v, want errEvalNoRows", name, s.RhoErr)
		}
	}
}

package metrics

import (
	"errors"
	"math"
	"slices"
	"time"
)

// The split and the statistics of the story-points evaluation (gt-wizcsy): the
// held-out test part of the landed rows, and for the blended estimate, each of
// its three inputs and a constant, how far the predicted minutes land from the
// polecat minutes the bead took. Printing them is gt-htz2cb.

// evalTestTenths is the test part in tenths of the rows: the newest three in
// ten.
const evalTestTenths = 3

// evalWithinFactor is the band the within share counts a prediction in: from
// 1/1.5 to 1.5 times the actual minutes.
const evalWithinFactor = 1.5

// errEvalNoRows reports a predictor that scored no test row, so every figure
// in its entry is undefined.
var errEvalNoRows = errors.New("metrics: the predictor scored no test row")

// The predictor names, in the order evalStats returns them.
const (
	predictorBlended  = "blended"
	predictorShape    = "shape"
	predictorEmbed    = "embed"
	predictorJev      = "jev"
	predictorConstant = "constant"
)

// PredictorStats is one predictor's held-out error over the test part, in
// minutes. A zero N means the predictor could score no test row, so every
// figure is undefined and a printer must say so rather than print a zero error.
// The log figures are over the N - SkippedLog rows with a positive actual and
// prediction, and are undefined when that count is zero.
type PredictorStats struct {
	Name       string  // blended, shape, embed, jev or constant
	N          int     // test rows the predictor could score
	SkippedLog int     // rows left out of the log figures: a non-positive actual or prediction
	MAE        float64 // mean absolute error over the N rows
	MedianAE   float64 // median absolute error over the N rows
	LogRMS     float64 // root mean square of ln(predicted/actual)
	LogBias    float64 // mean of ln(predicted/actual): positive says the predictor runs long
	Within     float64 // share of the rows with a positive actual inside the factor band
	WithinOK   bool    // false when no row could be placed in or out of the band
	Rho        float64 // rank correlation of the actual and predicted minutes
	RhoErr     error   // why there is no rank correlation; Rho is not meaningful when set
}

// splitEval divides rows into the training part and the test part the
// evaluation scores, both ordered by landed time. With a zero since the test
// part is the newest 30 percent of the rows, rounded to the nearest row; with a
// since, it is the rows landed at or after it. The caller's rows are not
// reordered.
func splitEval(rows []EvalRow, since time.Time) (train, test []EvalRow) {
	ordered := slices.Clone(rows)
	slices.SortStableFunc(ordered, func(a, b EvalRow) int { return a.LandedAt.Compare(b.LandedAt) })
	cut := len(ordered)
	if since.IsZero() {
		cut -= (len(ordered)*evalTestTenths + 5) / 10
	} else {
		for i, row := range ordered {
			if !row.LandedAt.Before(since) {
				cut = i
				break
			}
		}
	}
	// The three-index slices keep an append to one part from writing over the
	// other part's rows.
	return ordered[:cut:cut], ordered[cut:]
}

// evalStats scores every predictor over the test part: the blended estimate,
// its three inputs, and a constant equal to the median of the training rows'
// actual minutes. A row whose estimate carried no value for an input is not
// scored for that input. The command that prints the evaluation is gt-htz2cb;
// until it lands, the tests are this function's only caller.
func evalStats(train, test []EvalRow) []PredictorStats {
	constant, constantOK := trainingMedian(train)
	return []PredictorStats{
		scorePredictor(predictorBlended, test, func(row EvalRow) (float64, bool) {
			return row.BlendedMinutes, true
		}),
		scorePredictor(predictorShape, test, func(row EvalRow) (float64, bool) {
			return optional(row.ShapeMinutes)
		}),
		scorePredictor(predictorEmbed, test, func(row EvalRow) (float64, bool) {
			return optional(row.EmbedMinutes)
		}),
		scorePredictor(predictorJev, test, func(row EvalRow) (float64, bool) {
			return optional(row.JevMinutes)
		}),
		scorePredictor(predictorConstant, test, func(EvalRow) (float64, bool) {
			return constant, constantOK
		}),
	}
}

// optional reads an input's minutes, reporting false for a row whose estimate
// carried no such input.
func optional(v *float64) (float64, bool) {
	if v == nil {
		return 0, false
	}
	return *v, true
}

// trainingMedian is the median of the training rows' actual minutes, the
// constant predictor's value. It reports false for an empty training part,
// which has no median.
func trainingMedian(train []EvalRow) (float64, bool) {
	if len(train) == 0 {
		return 0, false
	}
	minutes := make([]float64, len(train))
	for i, row := range train {
		minutes[i] = row.ActualMinutes
	}
	slices.Sort(minutes)
	return *median(minutes), true
}

// scorePredictor measures one predictor over the rows that carry a value for
// it. The log figures use the rows whose actual and prediction are both
// positive; a non-positive prediction stays in the within share and counts as
// outside the band, so a wild prediction lowers the share rather than leaving
// it.
func scorePredictor(name string, rows []EvalRow, value func(EvalRow) (float64, bool)) PredictorStats {
	s := PredictorStats{Name: name}
	var actual, predicted []float64
	for _, row := range rows {
		p, ok := value(row)
		if !ok {
			continue
		}
		actual = append(actual, row.ActualMinutes)
		predicted = append(predicted, p)
	}
	s.N = len(actual)
	if s.N == 0 {
		s.RhoErr = errEvalNoRows
		return s
	}
	absErr := make([]float64, s.N)
	for i := range absErr {
		absErr[i] = math.Abs(predicted[i] - actual[i])
	}
	s.MAE = sampleMean(absErr)
	slices.Sort(absErr)
	s.MedianAE = *median(absErr)

	limit := math.Log(evalWithinFactor)
	var logs []float64
	within, bandN := 0, 0
	for i := range actual {
		// A log ratio needs both sides positive, so a row with a non-positive
		// actual or prediction is counted in SkippedLog and left out of the
		// log figures. An actual that is not positive has no factor band
		// either; a non-positive prediction is outside every band.
		if actual[i] <= 0 {
			s.SkippedLog++
			continue
		}
		bandN++
		if predicted[i] <= 0 {
			s.SkippedLog++
			continue
		}
		logRatio := math.Log(predicted[i] / actual[i])
		logs = append(logs, logRatio)
		if math.Abs(logRatio) <= limit {
			within++
		}
	}
	s.Within, s.WithinOK = share(within, bandN)
	if len(logs) > 0 {
		squared := make([]float64, len(logs))
		for i, logRatio := range logs {
			squared[i] = logRatio * logRatio
		}
		s.LogRMS = math.Sqrt(sampleMean(squared))
		s.LogBias = sampleMean(logs)
	}
	s.Rho, s.RhoErr = Spearman(actual, predicted)
	return s
}

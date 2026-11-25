package metrics

import "slices"

// evalBandEdges are the blended-minutes edges the bands split at, ascending. A
// row at an edge belongs to the band starting there, so 10 is in 10 to 10.5.
var evalBandEdges = []float64{6, 8, 9, 10, 10.5, 12}

// evalBandLabels names the bands evalBands returns, in order: one more label
// than there are edges.
var evalBandLabels = []string{
	"under 6", "6 to 8", "8 to 9", "9 to 10", "10 to 10.5", "10.5 to 12", "12 and over",
}

// evalOverTenHalf and evalOverTwenty are the actual-minutes thresholds the band
// shares count rows over.
const (
	evalOverTenHalf = 10.5
	evalOverTwenty  = 20
)

// EvalBand is one estimate band of the evaluation: the rows whose blended
// minutes fall in it, with the median of each minutes figure they carry and the
// share of them that ran long. A median is nil when no row in the band carries
// that figure, and each share is a fraction of Count, zero for an empty band.
type EvalBand struct {
	Label            string
	Count            int
	ActualMedian     *float64
	ShapeMedian      *float64
	EmbedMedian      *float64
	JevMedian        *float64
	BlendedMedian    *float64
	OverTenHalfShare float64
	OverTwentyShare  float64
}

// evalBands groups rows into the estimate bands, one EvalBand per label in
// evalBandLabels order, so an empty input still returns every band.
func evalBands(rows []EvalRow) []EvalBand {
	groups := make([][]EvalRow, len(evalBandLabels))
	for _, row := range rows {
		i := evalBandIndex(row.BlendedMinutes)
		groups[i] = append(groups[i], row)
	}
	bands := make([]EvalBand, len(groups))
	for i, group := range groups {
		bands[i] = evalBandOf(evalBandLabels[i], group)
	}
	return bands
}

// evalBandIndex is the band a blended estimate falls in: the number of edges at
// or below it.
func evalBandIndex(blended float64) int {
	i := 0
	for i < len(evalBandEdges) && evalBandEdges[i] <= blended {
		i++
	}
	return i
}

// evalBandOf summarizes one band's rows under its label.
func evalBandOf(label string, rows []EvalRow) EvalBand {
	band := EvalBand{
		Label:         label,
		Count:         len(rows),
		ActualMedian:  bandMedian(rows, func(r EvalRow) (float64, bool) { return r.ActualMinutes, true }),
		ShapeMedian:   bandMedian(rows, func(r EvalRow) (float64, bool) { return optional(r.ShapeMinutes) }),
		EmbedMedian:   bandMedian(rows, func(r EvalRow) (float64, bool) { return optional(r.EmbedMinutes) }),
		JevMedian:     bandMedian(rows, func(r EvalRow) (float64, bool) { return optional(r.JevMinutes) }),
		BlendedMedian: bandMedian(rows, func(r EvalRow) (float64, bool) { return r.BlendedMinutes, true }),
	}
	var overTenHalf, overTwenty int
	for _, r := range rows {
		if r.ActualMinutes > evalOverTenHalf {
			overTenHalf++
		}
		if r.ActualMinutes > evalOverTwenty {
			overTwenty++
		}
	}
	if len(rows) > 0 {
		band.OverTenHalfShare = float64(overTenHalf) / float64(len(rows))
		band.OverTwentyShare = float64(overTwenty) / float64(len(rows))
	}
	return band
}

// bandMedian is the median of the values value reports for rows, reporting
// false for a row that carries none. It is nil when no row carries a value.
func bandMedian(rows []EvalRow, value func(EvalRow) (float64, bool)) *float64 {
	var values []float64
	for _, r := range rows {
		if v, ok := value(r); ok {
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	return median(values)
}

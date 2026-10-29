package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

// This file reads an earlier extract's output directory back for a later report:
// the footer.json the extract wrote and its weekly.csv, typed for the metrics
// and render code.

const (
	footerName = "footer.json"
	weeklyName = "weekly.csv"
)

// Footer is the extract's footer.json: the rows it read and the ones it refused,
// so the report can say what a week's figures do not cover. RowsDropped and Gaps
// hold (source, reason) pairs.
type Footer struct {
	RowsRead          map[string]int `json:"rows_read"`
	RowsDropped       [][]string     `json:"rows_dropped"`
	Gaps              [][]string     `json:"gaps"`
	SourcesUnreadable []string       `json:"sources_unreadable"`
}

// Result is an earlier extract's output as LoadResult reads it back.
type Result struct {
	Weekly []WeeklyMetrics
	Footer Footer
}

// LoadResult reads outDir's weekly.csv as typed rows and its footer.json, so a
// report rebuilds what the extract published without rerunning it.
func LoadResult(env Env, outDir string) (Result, error) {
	data, err := env.ReadFile(filepath.Join(outDir, footerName))
	if err != nil {
		return Result{}, fmt.Errorf("reading %s: %w", footerName, err)
	}
	var footer Footer
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&footer); err != nil {
		return Result{}, fmt.Errorf("%s: %w", footerName, err)
	}
	// The Python's json.load reads one value and refuses a tail, so a second
	// value here means the file is corrupt rather than a footer with more in it.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Result{}, fmt.Errorf("%s: data after the top-level object", footerName)
	}

	data, err = env.ReadFile(filepath.Join(outDir, weeklyName))
	if err != nil {
		return Result{}, fmt.Errorf("reading %s: %w", weeklyName, err)
	}
	rows, err := ReadCSV(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", weeklyName, err)
	}
	weekly, err := TypedWeekly(rows)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", weeklyName, err)
	}
	return Result{Weekly: weekly, Footer: footer}, nil
}

// TypedWeekly returns rows' cells as the typed rows the metric and render code
// read, with an empty cell kept as the figure the week lacks.
func TypedWeekly(rows []Row) ([]WeeklyMetrics, error) {
	out := make([]WeeklyMetrics, 0, len(rows))
	for i, r := range rows {
		m, err := typedWeeklyRow(r)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i+1, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// typedWeeklyRow turns one weekly.csv row into the typed row it describes.
func typedWeeklyRow(r Row) (WeeklyMetrics, error) {
	var m WeeklyMetrics
	// The Python reads the label columns with r["..."], which raises KeyError
	// when the row lacks one, and the numeric columns with r.get(c, ""), which
	// makes an absent column the value the week lacks. The two arms differ on
	// purpose: requiredCell is r["..."], weeklyCellFloat is r.get(c, "").
	for _, c := range []struct {
		name string
		dst  *string
	}{{"week", &m.Week}, {"rig", &m.Rig}} {
		v, err := requiredCell(r, c.name)
		if err != nil {
			return WeeklyMetrics{}, err
		}
		*c.dst = v
	}
	thin, err := requiredCell(r, "thin")
	if err != nil {
		return WeeklyMetrics{}, err
	}
	m.Thin = thin == "True"
	model, err := requiredCell(r, "dominant_model")
	if err != nil {
		return WeeklyMetrics{}, err
	}
	if model != "" {
		m.DominantModel = &model
	}

	for _, c := range []struct {
		name string
		dst  **float64
	}{
		{"size_median", &m.SizeMedian},
		{"size_p90", &m.SizeP90},
		{"file_entropy_mean", &m.FileEntropyMean},
		{"package_entropy_mean", &m.PackageEntropyMean},
		{"attempts_mean", &m.AttemptsMean},
		{"rejected_share", &m.RejectedShare},
		{"first_try_share", &m.FirstTryShare},
		{"h_labels", &m.HLabels},
		{"h_rigs", &m.HRigs},
		{"h_types", &m.HTypes},
		{"handoff_bytes_median", &m.HandoffBytesMedian},
		{"sessions_median", &m.SessionsMedian},
		{"rho", &m.Rho},
		{"dependency_density", &m.DependencyDensity},
		{"conflict_gate_share", &m.ConflictGateShare},
		{"unknown_share", &m.UnknownShare},
		{"mode_shift", &m.ModeShift},
		{"om_threshold", &m.OMThreshold},
	} {
		v, err := weeklyCellFloat(r, c.name)
		if err != nil {
			return WeeklyMetrics{}, err
		}
		*c.dst = v
	}
	for _, c := range []struct {
		name string
		dst  *int
	}{
		{"landings", &m.Landings},
		{"unreadable", &m.Unreadable},
		{"manual_rescue_count", &m.ManualRescueCount},
		{"rejections", &m.Rejections},
		{"rho_numerator", &m.RhoNumerator},
		{"rho_denominator", &m.RhoDenominator},
		// The Python leaves this column a float after typing; WeeklyMetrics
		// stores it as the count the extract wrote without a decimal point.
		{"handoff_bytes_total", &m.HandoffBytesTotal},
	} {
		v, err := weeklyCellInt(r, c.name)
		if err != nil {
			return WeeklyMetrics{}, err
		}
		*c.dst = v
	}
	return m, nil
}

// requiredCell returns r's cell for a column typed_weekly reads as r[name]: a
// row without the column is an error, where the Python raises KeyError.
func requiredCell(r Row, name string) (string, error) {
	v, ok := r[name]
	if !ok {
		return "", fmt.Errorf("no %s column", name)
	}
	return v, nil
}

// weeklyCellFloat reads one numeric cell the way typed_weekly reads it with
// r.get(column, ""): an empty cell, or a column the row does not carry, is a
// figure the week lacks (the Python's None).
func weeklyCellFloat(r Row, column string) (*float64, error) {
	cell := r[column]
	if cell == "" {
		return nil, nil
	}
	// float() accepts a cell with surrounding whitespace, which a hand-edited
	// CSV can carry.
	v, err := strconv.ParseFloat(strings.TrimSpace(cell), 64)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", column, err)
	}
	return &v, nil
}

// weeklyCellInt reads one count cell, truncating a fractional one as the
// Python's int() does.
func weeklyCellInt(r Row, column string) (int, error) {
	v, err := weeklyCellFloat(r, column)
	if err != nil || v == nil {
		return 0, err
	}
	if !fitsInt(*v) {
		return 0, fmt.Errorf("%s: %v is not a count an int can hold", column, *v)
	}
	return int(*v), nil
}

// fitsInt reports whether v is a value an int can hold. The upper bound is
// exclusive because float64(math.MaxInt) rounds up to 2^63: a count of exactly
// 2^63 is out of an int's range, where the Python's arbitrary-precision int()
// would carry it. NaN and infinity fail the comparisons too.
func fitsInt(v float64) bool {
	return v >= float64(math.MinInt) && v < float64(math.MaxInt)
}

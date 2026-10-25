package metrics

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// mangoTargets are the landing outcomes mango explores, in the order the panel
// and the mango section report them. manual_rescue stays a column and a
// predictor of the other targets, not a target (gt-jkaxr).
var mangoTargets = []string{"rejected_before_landing", "reached_overrun_stop"}

// mangoIdentity are dropped from the predictors because each is unique or
// derived per landing: a box over one restates the row, not a pattern.
var mangoIdentity = []string{"bead", "landed_at", "week"}

// mangoDuplicate is dropped because it carries the same value as
// dominant_model at landing level.
var mangoDuplicate = []string{"model"}

// defaultTrainFraction is the share of the earliest landings that train a box;
// the rest are held out to check it.
const defaultTrainFraction = 0.7

// SplitByDate returns rows ordered by landed_at, cut so that train holds the
// earliest int(len(rows)*trainFraction + 1e-9) of them and holdout the rest.
// A fraction outside [0, 1] cuts at the nearer end of rows, and it is clamped
// before the conversion to an int: converting an out-of-range or non-finite
// float is implementation-defined. NaN has no nearer end, so it cuts at the
// start, which is an empty train.
func SplitByDate(rows []Row, trainFraction float64) (train, holdout []Row) {
	ordered := make([]Row, len(rows))
	copy(ordered, rows)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i]["landed_at"] < ordered[j]["landed_at"] })
	f := trainFraction
	switch {
	case math.IsNaN(f) || f < 0:
		f = 0
	case f > 1:
		f = 1
	}
	k := int(float64(len(ordered))*f + 1e-9)
	return ordered[:k], ordered[k:]
}

// ReadSidecar reads a landings.sidecar.json's explain_only list; keys the
// struct does not name are ignored.
func ReadSidecar(r io.Reader) (explainOnly []string, err error) {
	var sidecar struct {
		ExplainOnly []string `json:"explain_only"`
	}
	if err := json.NewDecoder(r).Decode(&sidecar); err != nil {
		return nil, fmt.Errorf("landings.sidecar.json: %w", err)
	}
	return sidecar.ExplainOnly, nil
}

// Prepared is one target's split of the landings, ready to write.
type Prepared struct {
	// Train and Holdout are rows ordered by landed_at, the earliest in Train.
	Train, Holdout []Row
	// Predictors are the header's columns that survive every drop rule, in
	// header order.
	Predictors []string
	// Columns are the columns WriteSplit writes: Predictors then the target.
	Columns []string
	// TrainPositives and HoldoutPositives count the rows whose target cell
	// reads positive.
	TrainPositives, HoldoutPositives int
	// LastPositiveAt is the latest landed_at among the positives, empty when
	// there is none.
	LastPositiveAt string
}

// Prepare splits rows at the default train fraction and computes the predictor
// columns and the target's positive counts.
//
// A column is dropped when the sidecar names it explain-only, when it is a
// target, an identity or the duplicate, or when it defines this target. A
// header column none of those name is carried as a predictor whether or not
// this package knows it.
func Prepare(header []string, rows []Row, explainOnly []string, target string) Prepared {
	train, holdout := SplitByDate(rows, defaultTrainFraction)
	drop := make(map[string]bool, len(explainOnly)+len(mangoTargets)+len(mangoIdentity)+len(mangoDuplicate))
	for _, c := range explainOnly {
		drop[c] = true
	}
	for _, c := range mangoTargets {
		drop[c] = true
	}
	for _, c := range mangoIdentity {
		drop[c] = true
	}
	for _, c := range mangoDuplicate {
		drop[c] = true
	}
	predictors := make([]string, 0, len(header))
	for _, c := range header {
		if !drop[c] {
			predictors = append(predictors, c)
		}
	}
	p := Prepared{
		Train:      train,
		Holdout:    holdout,
		Predictors: predictors,
		Columns:    append(append([]string{}, predictors...), target),
	}
	// landed_at sorts as text because the extract writes ISO timestamps.
	for _, r := range train {
		if positive(r[target]) {
			p.TrainPositives++
			p.LastPositiveAt = later(p.LastPositiveAt, r["landed_at"])
		}
	}
	for _, r := range holdout {
		if positive(r[target]) {
			p.HoldoutPositives++
			p.LastPositiveAt = later(p.LastPositiveAt, r["landed_at"])
		}
	}
	return p
}

// positive reads a target cell as the flag it is: the extract writes the three
// targets as int(flag), so "1" is the ordinary positive, and a cell another
// table wrote as a non-zero number or as a Python bool still counts.
func positive(cell string) bool {
	c := strings.TrimSpace(cell)
	if f, err := strconv.ParseFloat(c, 64); err == nil {
		return f != 0 && !math.IsNaN(f)
	}
	return strings.EqualFold(c, "true")
}

// later returns whichever of a and b is the later timestamp, treating "" as
// earlier than any timestamp.
func later(a, b string) string {
	if b > a {
		return b
	}
	return a
}

// WriteSplit writes train.csv and test.csv into dir, each holding p.Columns:
// the predictors in header order and then the target.
//
// w opens each path, so a caller can write to disk or keep the files in
// memory; a row's other cells, such as the explain-only columns, are not
// written.
func WriteSplit(p Prepared, dir string, w func(path string) (io.WriteCloser, error)) error {
	for _, f := range []struct {
		name string
		rows []Row
	}{
		{"train.csv", p.Train},
		{"test.csv", p.Holdout},
	} {
		path := filepath.Join(dir, f.name)
		if err := writeMangoCSV(path, p.Columns, f.rows, w); err != nil {
			return err
		}
	}
	return nil
}

// writeMangoCSV writes header and rows through the writer open returns, with
// the CRLF endings the Python run's csv module emits.
func writeMangoCSV(path string, header []string, rows []Row, open func(string) (io.WriteCloser, error)) error {
	f, err := open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	fail := func(err error) error {
		_ = f.Close()
		return fmt.Errorf("%s: %w", path, err)
	}
	cw := csv.NewWriter(f)
	cw.UseCRLF = true
	if err := cw.Write(header); err != nil {
		return fail(err)
	}
	rec := make([]string, len(header))
	for i, row := range rows {
		for j, name := range header {
			rec[j] = row[name]
		}
		if err := cw.Write(rec); err != nil {
			return fail(fmt.Errorf("row %d: %w", i+2, err))
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

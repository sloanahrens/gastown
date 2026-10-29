package metrics

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// The replication maths of the town's metrics: the checks that decide whether a
// box mango found on the training split also holds on the held-out split. The
// tests check it against the golden cases in testdata/replicate_cases.json.
//
// Range and Cube mirror the ranges in mango explore's result JSON: a range
// names a column and is either a set of categories or a numeric span.

// Range is one constraint of a box. Categories != nil means a category set;
// otherwise the range is the numeric span Min..Max.
type Range struct {
	Column     string   `json:"column"`
	Categories []string `json:"categories,omitempty"`
	Min        float64  `json:"min"`
	Max        float64  `json:"max"`
}

// Cube is a box: the rows that satisfy every one of its ranges.
type Cube struct {
	Ranges []Range `json:"ranges"`
}

// BinomTwoSided is the exact two-sided binomial test of k successes in n trials
// against success probability p: the sum of the probability of every outcome
// that is at most as likely as the observed one (compared in log space with a
// 1e-12 slack), capped at 1. With no trials the evidence is nil, so the answer
// is 1; a p of 0 or 1 leaves one possible outcome. A k outside [0, n] is a
// caller bug and returns NaN rather than a number that looks like evidence.
func BinomTwoSided(k, n int, p float64) float64 {
	if n == 0 {
		return 1
	}
	if k < 0 || k > n {
		return math.NaN()
	}
	if p <= 0 {
		if k == 0 {
			return 1
		}
		return 0
	}
	if p >= 1 {
		if k == n {
			return 1
		}
		return 0
	}
	lgN, _ := math.Lgamma(float64(n + 1))
	logs := make([]float64, n+1)
	for i := 0; i <= n; i++ {
		lgI, _ := math.Lgamma(float64(i + 1))
		lgNI, _ := math.Lgamma(float64(n - i + 1))
		logs[i] = lgN - lgI - lgNI + float64(i)*math.Log(p) + float64(n-i)*math.Log1p(-p)
	}
	observed := logs[k]
	total := 0.0
	for _, lp := range logs {
		if lp <= observed+1e-12 {
			total += math.Exp(lp)
		}
	}
	return math.Min(1, total)
}

// Holm returns the Holm step-down adjusted p-values, in the input order: the
// ranks are taken in ascending p (ties keep their input order), each value is
// scaled by the number of hypotheses not yet rejected, and the running maximum
// keeps the adjusted values monotone and at most 1.
func Holm(p []float64) []float64 {
	order := make([]int, len(p))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return p[order[a]] < p[order[b]] })
	adjusted := make([]float64, len(p))
	running := 0.0
	for rank, i := range order {
		running = math.Max(running, math.Min(1, float64(len(p)-rank)*p[i]))
		adjusted[i] = running
	}
	return adjusted
}

// Matches reports whether row satisfies every range of c. Columns are matched
// by name. A category range needs the cell text in the set. A numeric range
// needs a cell that parses as a number (surrounding spaces allowed) within
// Min..Max inclusive; an empty or unparseable cell never matches it. A NaN cell
// passes the span test, because NaN is neither below Min nor
// above Max. A column the row lacks reads as an empty cell, so it fails a
// numeric range and a category set that does not hold the empty string.
func Matches(c Cube, row map[string]string) bool {
	for _, r := range c.Ranges {
		cell := row[r.Column]
		if r.Categories != nil {
			found := false
			for _, category := range r.Categories {
				if category == cell {
					found = true
					break
				}
			}
			if !found {
				return false
			}
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(cell), 64)
		if err != nil {
			return false
		}
		if v < r.Min || v > r.Max {
			return false
		}
	}
	return true
}

// Describe renders a box the way the report prints it: ranges joined by "; ",
// a category set as "col in {a, b}", a point as "col = v" and a span as
// "col in [lo, hi]", numbers to six significant digits.
func Describe(c Cube) string {
	parts := make([]string, 0, len(c.Ranges))
	for _, r := range c.Ranges {
		switch {
		case r.Categories != nil:
			parts = append(parts, fmt.Sprintf("%s in {%s}", r.Column, strings.Join(r.Categories, ", ")))
		case r.Min == r.Max:
			parts = append(parts, fmt.Sprintf("%s = %.6g", r.Column, r.Min))
		default:
			parts = append(parts, fmt.Sprintf("%s in [%.6g, %.6g]", r.Column, r.Min, r.Max))
		}
	}
	return strings.Join(parts, "; ")
}

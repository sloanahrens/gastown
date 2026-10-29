package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// The mango stage: run the CLI over a target's training split and check the
// boxes it found on the holdout. The Markdown report is a separate step.

// The experiment's explore parameters. A zero MangoOptions asks for these, so
// a caller that wants the report's run passes the zero value.
const (
	MangoMinSupport   = 30
	MangoMinPurity    = 0.15
	MangoPermutations = 1000
	MangoSeed         = 20261019
)

// mangoOutputTail is how much of a failed command's output its error carries:
// the CLI reports the reason it stopped in the last lines.
const mangoOutputTail = 800

// mangoRegime are the columns that describe the om regime rather than a
// landing, so a box made only of them reads as a period, not a predictor.
var mangoRegime = map[string]bool{"om_threshold": true, "dominant_model": true}

// MangoOptions are an explore run's parameters and the seam it reads the
// result through. A zero field asks for the experiment's default, so the zero
// value is the run the report makes.
type MangoOptions struct {
	MinSupport   int     // mango explore's --min-size
	MinPurity    float64 // --min-purity
	Permutations int     // --permutations
	Seed         int64   // --seed
	// NoWiden omits --widen. The flag is on by default because small wide
	// tables otherwise yield no cube of min-size rows (ma-65b, ma-2g6).
	NoWiden bool
	// ReadFile reads the result JSON; nil reads the host's disk.
	ReadFile func(string) ([]byte, error)
}

// withDefaults fills every zero field with the experiment's default.
func (o MangoOptions) withDefaults() MangoOptions {
	if o.MinSupport == 0 {
		o.MinSupport = MangoMinSupport
	}
	if o.MinPurity == 0 {
		o.MinPurity = MangoMinPurity
	}
	if o.Permutations == 0 {
		o.Permutations = MangoPermutations
	}
	if o.Seed == 0 {
		o.Seed = MangoSeed
	}
	if o.ReadFile == nil {
		o.ReadFile = os.ReadFile
	}
	return o
}

// Box is one hypercube mango found on the training split: its ranges, the rows
// it matched there and their target rate.
type Box struct {
	Cube
	N      int     `json:"n"`
	Purity float64 `json:"purity"`
}

// SkippedColumn is a predictor mango never constrained, with its reason.
type SkippedColumn struct {
	Column string `json:"column"`
	Reason string `json:"reason"`
}

// MangoResult is what one explore run found and how much of the search space it
// covered, as the report's counters name them.
type MangoResult struct {
	Iterations     int
	Germs          int // mango's germsTried
	Candidates     int
	Generated      int
	Valid          int
	Significant    int
	SkippedColumns []SkippedColumn
	Boxes          []Box
}

// mangoResultFile is the subset of mango explore's result JSON this package
// reads; every other key the CLI writes is ignored.
type mangoResultFile struct {
	Hypercubes []Box `json:"hypercubes"`
	Stats      struct {
		Candidates int `json:"candidates"`
		Generate   struct {
			Iterations     int             `json:"iterations"`
			GermsTried     int             `json:"germsTried"`
			Generated      int             `json:"generated"`
			Valid          int             `json:"valid"`
			Significant    int             `json:"significant"`
			SkippedColumns []SkippedColumn `json:"skippedColumns"`
		} `json:"generate"`
	} `json:"stats"`
}

// RunMango runs mango ingest and explore over dir/train.csv and returns the
// boxes and counters of dir/result.json. A command that fails is an error
// carrying the last 800 bytes of its output.
func RunMango(ctx context.Context, run Runner, binary, dir, target string, o MangoOptions) (MangoResult, error) {
	o = o.withDefaults()
	parquet := filepath.Join(dir, "train.parquet")
	resultPath := filepath.Join(dir, "result.json")
	if err := mangoCall(ctx, run, "ingest", binary, "ingest", filepath.Join(dir, "train.csv"), "--out", parquet); err != nil {
		return MangoResult{}, err
	}
	explore := []string{
		binary, "explore", parquet,
		"--target", target,
		"--positive", "1",
		"--seed", strconv.FormatInt(o.Seed, 10),
		"--permutations", strconv.Itoa(o.Permutations),
		"--min-size", strconv.Itoa(o.MinSupport),
		"--min-purity", strconv.FormatFloat(o.MinPurity, 'g', -1, 64),
	}
	if !o.NoWiden {
		explore = append(explore, "--widen")
	}
	explore = append(explore, "--out", resultPath)
	if err := mangoCall(ctx, run, "explore", explore...); err != nil {
		return MangoResult{}, err
	}
	data, err := o.ReadFile(resultPath)
	if err != nil {
		return MangoResult{}, fmt.Errorf("mango result.json: %w", err)
	}
	var file mangoResultFile
	if err := json.Unmarshal(data, &file); err != nil {
		return MangoResult{}, fmt.Errorf("mango result.json: %w", err)
	}
	gen := file.Stats.Generate
	return MangoResult{
		Iterations:     gen.Iterations,
		Germs:          gen.GermsTried,
		Candidates:     file.Stats.Candidates,
		Generated:      gen.Generated,
		Valid:          gen.Valid,
		Significant:    gen.Significant,
		SkippedColumns: gen.SkippedColumns,
		Boxes:          file.Hypercubes,
	}, nil
}

// mangoCall runs one mango command and names the stage in the error, so a
// failure says which of ingest and explore stopped.
func mangoCall(ctx context.Context, run Runner, step string, argv ...string) error {
	out, err := run(ctx, argv)
	if err == nil {
		return nil
	}
	if tail := tailString(string(out), mangoOutputTail); tail != "" {
		return fmt.Errorf("mango %s failed: %w: %s", step, err, tail)
	}
	return fmt.Errorf("mango %s failed: %w", step, err)
}

// BoxCheck is one box measured on the holdout: the rows it matched there, the
// positives among them and their rate against the holdout's, the box's
// training numbers, the Holm-adjusted two-sided exact binomial p of the rate
// being the baseline, whether it replicates, and the "period" label when every
// range is on a regime column.
type BoxCheck struct {
	Box        string  // the box as Describe renders it
	TrainN     int     // rows the box matched on the training split
	TrainRate  float64 // the box's positive rate there
	N          int     // rows it matched on the holdout
	K          int     // positives among them
	Rate       float64 // K/N, zero when N is zero
	Baseline   float64 // the holdout's positive rate
	P          float64 // Holm-adjusted p
	Replicates bool
	Label      string
	// Cube is the box it was built from, so the panel can split it into its
	// single conditions.
	Cube Cube
}

// CheckHoldout measures every box on rows, the held-out split, against the
// target's positive rate there, and reports each box with the Holm-adjusted
// two-sided exact binomial p across the boxes. A box replicates when it matches
// at least minSupport rows, its rate is above the baseline and its p is below
// 0.05.
//
// The p-value is the two-sided exact binomial test, and a box must also sit
// above the baseline to replicate: that is two-sided evidence filtered by
// direction, not a one-sided test. A box significantly below the baseline is
// reported as not replicating rather than flagged, because boxes are searched
// for elevated rates.
func CheckHoldout(boxes []Box, rows []Row, target string, minSupport int) []BoxCheck {
	baseline := 0.0
	if len(rows) > 0 {
		positives := 0
		for _, row := range rows {
			if row[target] == "1" {
				positives++
			}
		}
		baseline = float64(positives) / float64(len(rows))
	}
	checks := make([]BoxCheck, len(boxes))
	pvalues := make([]float64, len(boxes))
	for i, box := range boxes {
		n, k := 0, 0
		for _, row := range rows {
			if Matches(box.Cube, row) {
				n++
				if row[target] == "1" {
					k++
				}
			}
		}
		p := 1.0
		if n > 0 {
			p = BinomTwoSided(k, n, baseline)
		}
		checks[i] = BoxCheck{
			Box:       Describe(box.Cube),
			Cube:      box.Cube,
			TrainN:    box.N,
			TrainRate: box.Purity,
			N:         n,
			K:         k,
			Baseline:  baseline,
			P:         p,
			Label:     boxLabel(box),
		}
		pvalues[i] = p
	}
	for i, p := range Holm(pvalues) {
		c := &checks[i]
		c.P = p
		if c.N > 0 {
			c.Rate = float64(c.K) / float64(c.N)
		}
		c.Replicates = c.N >= minSupport && c.Rate > c.Baseline && c.P < 0.05
	}
	return checks
}

// boxLabel is "period" when the box constrains at least one column and every
// column it constrains is a regime column.
func boxLabel(box Box) string {
	if len(box.Ranges) == 0 {
		return ""
	}
	for _, r := range box.Ranges {
		if !mangoRegime[r.Column] {
			return ""
		}
	}
	return "period"
}

// BestBox returns the box that best supports its target: of the boxes that
// replicate, the one with the highest holdout rate, else the one with the
// lowest Holm-adjusted p. It is nil when there are no boxes.
func BestBox(checks []BoxCheck) *BoxCheck {
	if len(checks) == 0 {
		return nil
	}
	best := checks[0]
	for _, c := range checks[1:] {
		switch {
		case c.Replicates != best.Replicates:
			if c.Replicates {
				best = c
			}
		case c.Replicates:
			if c.Rate > best.Rate {
				best = c
			}
		case c.P < best.P:
			best = c
		}
	}
	return &best
}

// TargetResult is one target's mango stage: the split's counts, how many boxes
// mango found and how many replicate, and the best of them.
type TargetResult struct {
	Target                     string
	TrainN, TrainPositives     int
	HoldoutN, HoldoutPositives int
	Boxes, Replicating         int
	Best                       *BoxCheck // nil when mango found no boxes
	// Conditions is the best box split into its single conditions, each with
	// its share of every prepared row; nil when mango found no boxes.
	Conditions []MangoCondition
	// LastPositiveAt is the split's latest landed_at among the positives, empty
	// when it holds none; the panel reports its day.
	LastPositiveAt string
}

// measureTarget pairs a prepared split with the boxes mango found on its
// training split, and splits the best of them into its conditions over every
// prepared row.
func measureTarget(target string, p Prepared, boxes []Box, minSupport int) TargetResult {
	checks := CheckHoldout(boxes, p.Holdout, target, minSupport)
	t := TargetResult{
		Target:           target,
		TrainN:           len(p.Train),
		TrainPositives:   p.TrainPositives,
		HoldoutN:         len(p.Holdout),
		HoldoutPositives: p.HoldoutPositives,
		Boxes:            len(boxes),
		Best:             BestBox(checks),
		LastPositiveAt:   p.LastPositiveAt,
	}
	for _, c := range checks {
		if c.Replicates {
			t.Replicating++
		}
	}
	if t.Best != nil {
		t.Conditions = MangoConditions(t.Best.Cube, preparedRows(p))
	}
	return t
}

// preparedRows is the target's rows together, train then holdout: a condition's
// share is over every landing the split prepared, not one side of it.
func preparedRows(p Prepared) []Row {
	rows := make([]Row, 0, len(p.Train)+len(p.Holdout))
	rows = append(rows, p.Train...)
	return append(rows, p.Holdout...)
}

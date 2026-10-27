package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The digest's "Mango findings" section: the weekly table's profile and, per
// target, the split's counts, the search's counters and the boxes measured on
// the holdout. It is a port of the Markdown section of
// scripts/town_metrics/mango_run.py.

// MangoSectionMinPositives is the fewest positives a target's training split
// needs for the section to run it; a thinner target is named and skipped,
// because a search over a handful of positives is not evidence either way.
const MangoSectionMinPositives = 30

// mangoProfileOutputTail bounds the failed profile the section quotes: the
// CLI says why it stopped in its last lines.
const mangoProfileOutputTail = 400

// mangoExplainNumeric are the post-hoc columns a box is explained with: each
// one's mean inside the box against the whole holdout's. They are read only
// after the landing, so they explain a box and never predict one.
var mangoExplainNumeric = []string{"lines_total", "files_touched", "file_entropy", "package_entropy", "attempts"}

// ExplainMean is one explain column's mean inside a box's holdout rows and
// across the whole holdout; a nil mean is no numeric row to average, which the
// section writes "n/a".
type ExplainMean struct {
	Box, All *float64
}

// CheckedBox is a BoxCheck with the post-hoc columns' means beside it.
type CheckedBox struct {
	BoxCheck
	// Explain holds one ExplainMean per column the section asked about.
	Explain map[string]ExplainMean
}

// CheckHoldoutExplained measures boxes on rows as CheckHoldout does, and adds
// each explain column's means to every check: inside the box's rows and
// across all of them.
//
// The means are for reading a box, never for predicting: a column that is only
// known after the landing cannot be a predictor, so it stays out of the split
// and is reported beside the box instead.
//
// A non-blank cell that is not a number is an error, as the Python's float()
// raises on it: the extract wrote something the mean cannot read, and
// dropping the cell would quietly move the mean instead of saying so.
func CheckHoldoutExplained(boxes []Box, rows []Row, target string, minSupport int,
	explain []string) ([]CheckedBox, error) {
	checks := CheckHoldout(boxes, rows, target, minSupport)
	all := make(map[string]*float64, len(explain))
	for _, column := range explain {
		mean, err := mangoMean(rows, column)
		if err != nil {
			return nil, err
		}
		all[column] = mean
	}
	out := make([]CheckedBox, len(checks))
	for i, check := range checks {
		hit := make([]Row, 0, check.N)
		for _, row := range rows {
			if Matches(boxes[i].Cube, row) {
				hit = append(hit, row)
			}
		}
		means := make(map[string]ExplainMean, len(explain))
		for _, column := range explain {
			mean, err := mangoMean(hit, column)
			if err != nil {
				return nil, err
			}
			means[column] = ExplainMean{Box: mean, All: all[column]}
		}
		out[i] = CheckedBox{BoxCheck: check, Explain: means}
	}
	return out, nil
}

// mangoMean is the mean of column's numeric cells in rows, and nil when none
// of them carries a number: the explain columns come from the extract, so a
// blank cell is a landing with no reading rather than a zero. A cell that is
// not a number is an error naming the column and the cell.
func mangoMean(rows []Row, column string) (*float64, error) {
	sum, n := 0.0, 0
	for _, row := range rows {
		cell := strings.TrimSpace(row[column])
		if cell == "" {
			continue
		}
		v, err := strconv.ParseFloat(cell, 64)
		if err != nil {
			return nil, fmt.Errorf("explain column %s: %q is not a number", column, cell)
		}
		sum += v
		n++
	}
	if n == 0 {
		return nil, nil
	}
	mean := sum / float64(n)
	return &mean, nil
}

// MangoSectionOptions are the section's two thresholds. A zero field asks for
// the experiment's default, so the zero value is the run the digest makes.
type MangoSectionOptions struct {
	// MinSupport is mango explore's --min-size; zero is MangoMinSupport.
	MinSupport int
	// MinPositives is the fewest positives a target's training split needs to
	// be run; zero is MangoSectionMinPositives.
	MinPositives int
	// Since windows the first-pass table to beads merged at or after it; zero
	// is every landing the logs hold.
	Since time.Time
}

// MangoSection renders the digest's "Mango findings" text: the weekly table's
// profile and, per target, the split's counts, the search's counters and the
// boxes measured on the holdout. It ends with the rejection tables
// MangoRejectionsSection renders and the first-pass table
// MangoFirstPassSection renders, so the digest carries the refusal rate and
// the author kinds' first-pass rate beside the findings they explain.
//
// It reads outDir's landings.csv and landings.sidecar.json, runs mango over
// outDir's weekly.csv and each run target's split through env.Run, writes the
// split under outDir/mango and the counts the backtest reads to
// outDir/mango-summary.json. It then reads the town's review log, each rig's
// landing log and the merged beads' notes, so the tables it ends with are the
// digest's own reading rather than a second copy of gt land om-stats. An empty
// binary is a host without mango: the text says so and names the directory
// holding the CSVs, so a report still renders.
func MangoSection(ctx context.Context, env Env, outDir, binary string, o MangoSectionOptions) (string, error) {
	if o.MinSupport == 0 {
		o.MinSupport = MangoMinSupport
	}
	if o.MinPositives == 0 {
		o.MinPositives = MangoSectionMinPositives
	}
	if binary == "" {
		return fmt.Sprintf("mango binary not found; CSVs written to %s, mango step skipped.", outDir), nil
	}
	switch {
	case env.Town == "":
		return "", errors.New("mango section: Env.Town is unset")
	case env.ReadFile == nil:
		return "", errors.New("mango section: Env.ReadFile is unset")
	case env.Run == nil:
		return "", errors.New("mango section: Env.Run is unset")
	}
	landings, err := env.ReadFile(filepath.Join(outDir, landingsName))
	if err != nil {
		return "", fmt.Errorf("%s: %w", landingsName, err)
	}
	header, rows, err := ReadLandings(bytes.NewReader(landings))
	if err != nil {
		return "", err
	}
	side, err := env.ReadFile(filepath.Join(outDir, sidecarName))
	if err != nil {
		return "", fmt.Errorf("%s: %w", sidecarName, err)
	}
	explainOnly, err := ReadSidecar(bytes.NewReader(side))
	if err != nil {
		return "", err
	}

	parts := make([]string, 0, len(mangoTargets)+1)
	parts = append(parts, "### Weekly table profile\n\n"+mangoProfile(ctx, env, outDir, binary))
	summary := make(map[string]MangoSummary, len(mangoTargets))
	for _, target := range mangoTargets {
		p := Prepare(header, rows, explainOnly, target)
		if p.TrainPositives < o.MinPositives {
			part := append(mangoHead(target, p),
				fmt.Sprintf("Skipped: fewer than %d positives in train.", o.MinPositives))
			parts = append(parts, strings.Join(part, "\n\n"))
			continue
		}
		runDir := filepath.Join(outDir, "mango", target)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			return "", fmt.Errorf("mango %s: %w", target, err)
		}
		if err := WriteSplit(p, runDir, mangoCreateFile); err != nil {
			return "", fmt.Errorf("mango %s split: %w", target, err)
		}
		out, err := RunMango(ctx, env.Run, binary, runDir, target,
			MangoOptions{MinSupport: o.MinSupport, ReadFile: env.ReadFile})
		if err != nil {
			parts = append(parts, strings.Join(append(mangoHead(target, p), err.Error()), "\n\n"))
			continue
		}
		var checks []CheckedBox
		if len(out.Boxes) > 0 && p.HoldoutPositives > 0 {
			checks, err = CheckHoldoutExplained(out.Boxes, p.Holdout, target, o.MinSupport, mangoExplainNumeric)
			if err != nil {
				return "", fmt.Errorf("mango %s: %w", target, err)
			}
		}
		replicating := 0
		for _, c := range checks {
			if c.Replicates {
				replicating++
			}
		}
		summary[target] = MangoSummary{Boxes: len(out.Boxes), Replicating: replicating, HoldoutPositives: p.HoldoutPositives}
		parts = append(parts, mangoTargetPart(target, p, out, checks))
	}
	if err := writeMangoSummary(filepath.Join(outDir, "mango-summary.json"), summary); err != nil {
		return "", err
	}
	records, skippedReviews, err := townReviewRecords(env)
	if err != nil {
		return "", fmt.Errorf("mango section: %w", err)
	}
	rejections, err := mangoRejectionsSection(ctx, env, records, skippedReviews)
	if err != nil {
		return "", fmt.Errorf("mango section: %w", err)
	}
	firstPass, err := mangoFirstPassSection(ctx, env, o.Since, records, skippedReviews)
	if err != nil {
		return "", fmt.Errorf("mango section: %w", err)
	}
	return strings.Join(parts, "\n\n") + "\n\n" + rejections + "\n\n" + firstPass, nil
}

// mangoProfile is the weekly table's profile: mango's output in a code block,
// with the local run directory replaced so a published report does not name
// the host that made it, or the failure that stopped it.
func mangoProfile(ctx context.Context, env Env, outDir, binary string) string {
	out, err := env.Run(ctx, []string{binary, "profile", filepath.Join(outDir, "weekly.csv")})
	if err != nil {
		failure := tailString(err.Error(), mangoProfileOutputTail)
		return "mango profile failed: " + strings.ReplaceAll(failure, outDir, "<run>")
	}
	text := strings.TrimRight(string(out), " \t\r\n\v\f")
	return "```\n" + strings.ReplaceAll(text, outDir, "<run>") + "\n```"
}

// mangoCreateFile is WriteSplit's opener: a target's split goes to disk under
// the run directory the section made for it.
func mangoCreateFile(path string) (io.WriteCloser, error) {
	return os.Create(path)
}

// writeMangoSummary writes the per-target counts backtest reads, with a
// skipped or failed target absent rather than present as a zero result, as
// the Python writes only the targets whose search ran. It is written after
// every target, so a failed weekly profile still leaves the targets' counts on
// disk beside the section that reports the failure.
func writeMangoSummary(path string, summary map[string]MangoSummary) error {
	data, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("mango-summary.json: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("mango-summary.json: %w", err)
	}
	return nil
}

// mangoHead is a target's heading and the split's counts, with the day the
// last positive landed when the split holds one.
func mangoHead(target string, p Prepared) []string {
	lines := []string{
		"### " + target,
		fmt.Sprintf("Train: %d rows, %d positive. Holdout: %d rows, %d positive.",
			len(p.Train), p.TrainPositives, len(p.Holdout), p.HoldoutPositives),
	}
	if p.LastPositiveAt != "" {
		last := p.LastPositiveAt
		if len(last) > 10 {
			last = last[:10]
		}
		lines = append(lines, "Last positive landed "+last+".")
	}
	return lines
}

// mangoTargetPart is one target's part of the section: its head, the search's
// counters, the reason its boxes cannot be checked when they cannot, and the
// table of boxes measured on the holdout.
func mangoTargetPart(target string, p Prepared, out MangoResult, checks []CheckedBox) string {
	lines := mangoHead(target, p)
	lines = append(lines, fmt.Sprintf("Search: %d cubes generated, %d valid, %d significant, %d tested "+
		"(%d iterations, %d germs tried).", out.Generated, out.Valid, out.Significant, out.Candidates,
		out.Iterations, out.Germs))
	if len(out.SkippedColumns) > 0 {
		named := make([]string, len(out.SkippedColumns))
		for i, c := range out.SkippedColumns {
			named[i] = fmt.Sprintf("%s (%s)", c.Column, c.Reason)
		}
		lines = append(lines, "Columns no box constrained: "+strings.Join(named, "; ")+".")
	}
	if len(out.Boxes) == 0 {
		lines = append(lines, fmt.Sprintf("No boxes: %d cubes generated, %d valid. When valid is 0 the generator "+
			"never reached the minimum support, which is not evidence that nothing is predictive.",
			out.Generated, out.Valid))
		return strings.Join(lines, "\n\n")
	}
	if p.HoldoutPositives == 0 {
		lines = append(lines, fmt.Sprintf("Holdout has no positives, so the %d box(es) found on train cannot be "+
			"checked on this split. The outcome stopped before the holdout period: that is a regime change in "+
			"itself, not a result.", len(out.Boxes)))
		return strings.Join(lines, "\n\n")
	}
	lines = append(lines, mangoTable(checks))
	return strings.Join(lines, "\n\n")
}

// mangoTable is the checks as the report's Markdown table: each box's
// training and holdout rates against the baseline, its Holm-adjusted p,
// whether it replicates, and its explain columns' means beside it.
func mangoTable(checks []CheckedBox) string {
	columns := mangoExplainColumns(checks)
	header := []string{"#", "box", "label", "train n", "train rate", "holdout n", "holdout rate",
		"baseline", "p (Holm)", "replicates"}
	for _, c := range columns {
		header = append(header, c+" (box vs all)")
	}
	rows := []string{
		"| " + strings.Join(header, " | ") + " |",
		"|" + strings.Repeat("---|", len(header)),
	}
	for i, ch := range checks {
		label := ch.Label
		if label == "" {
			label = "-"
		}
		cells := []string{
			strconv.Itoa(i + 1), ch.Box, label, mangoFmt(ch.TrainN, false), mangoFmt(ch.TrainRate, true),
			strconv.Itoa(ch.N), mangoFmt(ch.Rate, true), mangoFmt(ch.Baseline, true),
			fmt.Sprintf("%.3f", ch.P), mangoYesNo(ch.Replicates),
		}
		for _, c := range columns {
			mean := ch.Explain[c]
			cells = append(cells, mangoFmt(mean.Box, false)+" vs "+mangoFmt(mean.All, false))
		}
		rows = append(rows, "| "+strings.Join(cells, " | ")+" |")
	}
	return strings.Join(rows, "\n")
}

// mangoExplainColumns is every explain column the checks carry, sorted so the
// table's columns do not move with the map order.
func mangoExplainColumns(checks []CheckedBox) []string {
	seen := make(map[string]bool)
	var columns []string
	for _, ch := range checks {
		for c := range ch.Explain {
			if !seen[c] {
				seen[c] = true
				columns = append(columns, c)
			}
		}
	}
	sort.Strings(columns)
	return columns
}

// mangoFmt is one table cell: "n/a" for a value that is not there, a ratio as
// a percentage to one decimal, a float to three significant digits, and an int
// as itself.
func mangoFmt(v any, pct bool) string {
	switch x := v.(type) {
	case *float64:
		if x == nil {
			return "n/a"
		}
		return mangoFmt(*x, pct)
	case float64:
		if pct {
			return fmt.Sprintf("%.1f%%", 100*x)
		}
		return fmt.Sprintf("%.3g", x)
	case int:
		return strconv.Itoa(x)
	case nil:
		return "n/a"
	default:
		return fmt.Sprint(v)
	}
}

// mangoYesNo renders a check's replication the way the table does.
func mangoYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

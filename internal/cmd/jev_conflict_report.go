package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/jev/conflict"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/workspace"
)

// jevConflictCuts are the same_files probabilities the report reads at. A pair
// is at a cut when Jev's answer is at or above it.
var jevConflictCuts = []float64{0.6, 0.8}

// jevConflictDefaultSince is the window the report reads when --since is not
// given.
const jevConflictDefaultSince = "14d"

// jevWilsonZ is the two-sided 95% z-score, the one the metrics panel uses.
const jevWilsonZ = 1.96

var (
	jevConflictReportSince string
	jevConflictReportJSON  bool
)

var jevConflictReportCmd = &cobra.Command{
	Use:   "conflict-report",
	Short: "Measure the conflict battery's same-files answers as a dispatch warning would have",
	Long: `Read the conflict battery's log and count its same_files answers as a
dispatch advisory would have warned about them. Each bead pair counts once, at
its newest answer logged before the later of its two beads landed, so an answer
logged after the outcome is never used. A pair with a bead that never landed
counts its newest answer. The window is the answers logged in the --since
window ending now.

For the cuts 0.6 and 0.8 the report prints, per UTC day, the pairs at or above
the cut, how many of them are labeled, how many overlapped, and the precision.
The total row carries a 95% Wilson interval on the precision, and the mean
pairs per day is the warning volume. The mean divides by the window's length,
so a log younger than the window reads low.

It makes no Jev call, changes no bead and never fetches. The labels come from
the conflict adapter, which reads the rig's landings and the bare clone as it
stands. A pair whose commits the clone does not hold is counted as unlabeled,
and so is a pair whose answer stays below 0.6, which no cut reaches.

Examples:
  gt jev conflict-report
  gt jev conflict-report --since 7d --json`,
	Args: cobra.NoArgs,
	RunE: runJevConflictReport,
}

func init() {
	f := jevConflictReportCmd.Flags()
	f.StringVar(&jevConflictReportSince, "since", jevConflictDefaultSince, "Count answers logged in this window ending now: Nd or a Go duration such as 36h")
	f.BoolVar(&jevConflictReportJSON, "json", false, "Print the report as JSON")
	jevCmd.AddCommand(jevConflictReportCmd)
}

func runJevConflictReport(cmd *cobra.Command, _ []string) error {
	return jevConflictReportRun(cmd.Context(), cmd.OutOrStdout(), workspace.FindFromCwdOrError,
		jevConflictReportSince, jevConflictReportJSON, time.Now())
}

// jevConflictReportRun finds the town and runs the report in it. findTown is a
// parameter so a test can stand in for a cwd outside any town.
func jevConflictReportRun(ctx context.Context, w io.Writer, findTown func() (string, error), since string, asJSON bool, now time.Time) error {
	townRoot, err := findTown()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return jevConflictReportWith(ctx, w, townRoot, since, asJSON, now, scorecard.OpenReadOnly, jevConflictLandings)
}

// jevConflictOpener opens a battery's adapter. It is a parameter so a test can
// wrap the conflict adapter's labels.
type jevConflictOpener func(battery, townRoot string) (scorecard.Adapter, error)

// jevLandedAt reports when a bead landed: the stamp of its latest landing
// record, and false when it never landed.
type jevLandedAt func(bead string) (time.Time, bool, error)

// jevConflictLandingsOpener opens the landing times the report compares
// against. It is a parameter so a test can put fixed landings behind it.
type jevConflictLandingsOpener func(townRoot string) (jevLandedAt, error)

// jevConflictReportWith runs the report for the window ending at now. A read
// that fails fails the whole report, so a label or landing that cannot be read
// is never taken as clean.
func jevConflictReportWith(ctx context.Context, w io.Writer, townRoot, since string, asJSON bool, now time.Time,
	open jevConflictOpener, landings jevConflictLandingsOpener) error {
	if since == "" {
		since = jevConflictDefaultSince
	}
	from, err := parseSince(since, now)
	if err != nil {
		return err
	}
	a, err := open(scorecard.ConflictBattery, townRoot)
	if err != nil {
		return err
	}
	entries, skipped, err := jevConflictRead(a)
	if err != nil {
		return err
	}
	landed, err := landings(townRoot)
	if err != nil {
		return err
	}
	rep, err := jevConflictBuild(ctx, a, entries, landed, from, now)
	if err != nil {
		return err
	}
	rep.LinesSkipped = skipped
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return writeJevConflictText(w, rep)
}

// jevConflictEntry is one parsed line of the conflict log: the adapter's item,
// the pair's two beads, and when the answer was logged.
type jevConflictEntry struct {
	item      scorecard.Item
	bead      string
	other     string
	at        time.Time
	sameFiles float64
}

// jevConflictRead parses the conflict log through the adapter. A line that does
// not parse is skipped and counted, as the scorecard skips it. A missing log
// holds no pairs.
func jevConflictRead(a scorecard.Adapter) ([]jevConflictEntry, int, error) {
	f, err := os.Open(a.LogPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("opening the conflict log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var entries []jevConflictEntry
	skipped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var l conflict.Line
		if err := json.Unmarshal(raw, &l); err != nil {
			skipped++
			continue
		}
		item, err := a.Parse(raw)
		if err != nil {
			skipped++
			continue
		}
		entries = append(entries, jevConflictEntry{
			item:      item,
			bead:      l.Bead,
			other:     l.Other,
			at:        l.TS,
			sameFiles: l.Answers.SameFiles,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading the conflict log: %w", err)
	}
	return entries, skipped, nil
}

// jevConflictPairKey names a bead pair whatever order its two beads were
// written in, since a pair can be logged with either bead first.
func jevConflictPairKey(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "+" + b
}

// jevPairCutoff is the stamp an answer must precede to count: the later landing
// of the pair's two beads. ok is false when either bead never landed, and then
// every answer counts.
func jevPairCutoff(a, b string, landed jevLandedAt) (time.Time, bool, error) {
	ta, okA, err := landed(a)
	if err != nil {
		return time.Time{}, false, err
	}
	tb, okB, err := landed(b)
	if err != nil {
		return time.Time{}, false, err
	}
	if !okA || !okB {
		return time.Time{}, false, nil
	}
	if tb.After(ta) {
		ta = tb
	}
	return ta, true, nil
}

// jevConflictCounted is the one answer a bead pair counts at.
type jevConflictCounted struct {
	item      scorecard.Item
	at        time.Time
	sameFiles float64
}

// jevConflictPick chooses each bead pair's newest answer logged before the
// pair's cutoff, and keeps the pair when that answer falls in the window. Pairs
// are visited in key order, so the labels are asked in the same order on every
// run.
func jevConflictPick(entries []jevConflictEntry, landed jevLandedAt, from time.Time) ([]jevConflictCounted, error) {
	groups := map[string][]jevConflictEntry{}
	var keys []string
	for _, e := range entries {
		k := jevConflictPairKey(e.bead, e.other)
		if _, seen := groups[k]; !seen {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], e)
	}
	sort.Strings(keys)

	var counted []jevConflictCounted
	for _, k := range keys {
		es := groups[k]
		cutoff, bounded, err := jevPairCutoff(es[0].bead, es[0].other, landed)
		if err != nil {
			return nil, err
		}
		var best *jevConflictEntry
		for i := range es {
			e := &es[i]
			if bounded && !e.at.Before(cutoff) {
				continue
			}
			if best == nil || !e.at.Before(best.at) {
				best = e
			}
		}
		if best == nil || best.at.Before(from) {
			continue
		}
		counted = append(counted, jevConflictCounted{item: best.item, at: best.at, sameFiles: best.sameFiles})
	}
	return counted, nil
}

// jevConflictCell is the counts for one day, or for the whole window, at one
// cut.
type jevConflictCell struct {
	Pairs      int      `json:"pairs"`
	Labeled    int      `json:"labeled"`
	Unlabeled  int      `json:"unlabeled"`
	Overlapped int      `json:"overlapped"`
	Precision  *float64 `json:"precision"`
}

// finish sets the precision over the labeled pairs. It is nil when none is
// labeled, since a precision needs a denominator.
func (c *jevConflictCell) finish() {
	if c.Labeled == 0 {
		c.Precision = nil
		return
	}
	p := float64(c.Overlapped) / float64(c.Labeled)
	c.Precision = &p
}

type jevConflictDay struct {
	Day string `json:"day"`
	jevConflictCell
}

type jevConflictTotal struct {
	jevConflictCell
	WilsonLow       *float64 `json:"wilson_low"`
	WilsonHigh      *float64 `json:"wilson_high"`
	MeanPairsPerDay float64  `json:"mean_pairs_per_day"`
}

type jevConflictCut struct {
	Cut   float64          `json:"cut"`
	Days  []jevConflictDay `json:"days"`
	Total jevConflictTotal `json:"total"`
}

// jevConflictReaches reports whether a same_files answer is at or above at least
// one cut, so the pair is counted at some cut.
func jevConflictReaches(sameFiles float64) bool {
	for _, cut := range jevConflictCuts {
		if sameFiles >= cut {
			return true
		}
	}
	return false
}

// jevConflictReport is the report, in the shape --json prints.
type jevConflictReport struct {
	Since        time.Time        `json:"since"`
	WindowDays   float64          `json:"window_days"`
	Pairs        int              `json:"pairs"`
	LinesSkipped int              `json:"lines_skipped"`
	Cuts         []jevConflictCut `json:"cuts"`
}

// jevConflictBuild labels each counted pair and counts it at each cut it
// reaches. A label error fails the build.
func jevConflictBuild(ctx context.Context, a scorecard.Adapter, entries []jevConflictEntry, landed jevLandedAt,
	from, now time.Time) (jevConflictReport, error) {
	counted, err := jevConflictPick(entries, landed, from)
	if err != nil {
		return jevConflictReport{}, err
	}
	qSameFiles := scorecard.QuestionID(conflict.QuestionSameFiles)
	type labeled struct {
		c     jevConflictCounted
		label bool
		ok    bool
	}
	labels := make([]labeled, 0, len(counted))
	for _, c := range counted {
		// A pair below every cut is counted at none, so it is never labeled: a
		// label that cannot be read for it must not fail the report.
		if !jevConflictReaches(c.sameFiles) {
			continue
		}
		lab, err := a.Label(ctx, c.item)
		if err != nil {
			return jevConflictReport{}, fmt.Errorf("labeling pair %s: %w", c.item.ID, err)
		}
		v, ok := lab.Labels[qSameFiles]
		labels = append(labels, labeled{c: c, label: v, ok: lab.Labeled && ok})
	}

	windowDays := now.Sub(from).Hours() / 24
	rep := jevConflictReport{Since: from, WindowDays: windowDays, Pairs: len(counted)}
	for _, cut := range jevConflictCuts {
		days := map[string]*jevConflictCell{}
		var total jevConflictCell
		for _, l := range labels {
			if l.c.sameFiles < cut {
				continue
			}
			day := l.c.at.UTC().Format("2006-01-02")
			cell, ok := days[day]
			if !ok {
				cell = &jevConflictCell{}
				days[day] = cell
			}
			for _, c := range []*jevConflictCell{cell, &total} {
				c.Pairs++
				if !l.ok {
					c.Unlabeled++
					continue
				}
				c.Labeled++
				if l.label {
					c.Overlapped++
				}
			}
		}
		names := make([]string, 0, len(days))
		for d := range days {
			names = append(names, d)
		}
		sort.Strings(names)
		out := jevConflictCut{Cut: cut, Days: make([]jevConflictDay, 0, len(names))}
		for _, d := range names {
			days[d].finish()
			out.Days = append(out.Days, jevConflictDay{Day: d, jevConflictCell: *days[d]})
		}
		total.finish()
		out.Total = jevConflictTotal{jevConflictCell: total, MeanPairsPerDay: float64(total.Pairs) / windowDays}
		if low, high, ok := jevWilson(total.Overlapped, total.Labeled); ok {
			out.Total.WilsonLow, out.Total.WilsonHigh = &low, &high
		}
		rep.Cuts = append(rep.Cuts, out)
	}
	return rep, nil
}

// jevConflictLandings opens the landings of the rig the conflict battery reads.
func jevConflictLandings(townRoot string) (jevLandedAt, error) {
	return jevConflictLandingsOf(townRoot, conflict.RigName)
}

// jevConflictLandingsOf opens the rig's landings. Each bead's record is read
// once per report, and a bead with no record never landed.
func jevConflictLandingsOf(townRoot, rig string) (jevLandedAt, error) {
	f, err := land.RigLandingsFile(townRoot, rig)
	if err != nil {
		return nil, err
	}
	// A missing file reads every bead as never landed, so every answer would
	// count, including answers logged after the beads landed. The report refuses
	// instead.
	if _, err := os.Stat(f.Path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("the landings file %s does not exist, so the report cannot tell which answers came before a landing", f.Path)
		}
		return nil, fmt.Errorf("reading the landings file %s: %w", f.Path, err)
	}
	type landing struct {
		at    time.Time
		found bool
	}
	seen := map[string]landing{}
	return func(bead string) (time.Time, bool, error) {
		if l, ok := seen[bead]; ok {
			return l.at, l.found, nil
		}
		rec, found, err := f.LatestForBead(bead)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("reading the landings of %s: %w", bead, err)
		}
		seen[bead] = landing{at: rec.LandedAt, found: found}
		return rec.LandedAt, found, nil
	}, nil
}

// jevWilson is the 95% Wilson score interval for k of n, and false when n is
// zero. It repeats metrics.wilsonInterval, which is unexported.
func jevWilson(k, n int) (low, high float64, ok bool) {
	if n == 0 {
		return 0, 0, false
	}
	nf := float64(n)
	p := float64(k) / nf
	z2 := jevWilsonZ * jevWilsonZ
	denom := 1 + z2/nf
	center := (p + z2/(2*nf)) / denom
	half := jevWilsonZ * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf)) / denom
	return math.Max(0, center-half), math.Min(1, center+half), true
}

// writeJevConflictText prints one table per cut, then the total line. The
// columns are fixed width, so the text is built in full before it is written.
func writeJevConflictText(w io.Writer, rep jevConflictReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "conflict-report: same_files answers, one per bead pair, over %.1f days from %s (UTC)\n",
		rep.WindowDays, rep.Since.UTC().Format("2006-01-02"))
	fmt.Fprintf(&b, "pairs counted %d, lines skipped %d\n", rep.Pairs, rep.LinesSkipped)
	for _, c := range rep.Cuts {
		fmt.Fprintf(&b, "\ncut %.1f\n", c.Cut)
		fmt.Fprintf(&b, "%-10s  %6s  %7s  %9s  %10s  %9s\n", "DAY", "PAIRS", "LABELED", "UNLABELED", "OVERLAPPED", "PRECISION")
		for _, d := range c.Days {
			fmt.Fprintf(&b, "%-10s  %6d  %7d  %9d  %10d  %9s\n", d.Day, d.Pairs, d.Labeled, d.Unlabeled, d.Overlapped, jevPrecisionText(d.Precision))
		}
		t := c.Total
		wilson := "-"
		if t.WilsonLow != nil {
			wilson = fmt.Sprintf("%.3f-%.3f", *t.WilsonLow, *t.WilsonHigh)
		}
		fmt.Fprintf(&b, "total: pairs %d, labeled %d, unlabeled %d, overlapped %d, precision %s, 95%% Wilson %s\n",
			t.Pairs, t.Labeled, t.Unlabeled, t.Overlapped, jevPrecisionText(t.Precision), wilson)
		fmt.Fprintf(&b, "mean pairs per day: %.2f\n", t.MeanPairsPerDay)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// jevPrecisionText prints a precision to three places, or "-" with no labeled
// pair to measure it over.
func jevPrecisionText(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.3f", *p)
}

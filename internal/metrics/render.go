package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// This file renders the report: the Markdown digest a run publishes, the "what
// moved" comparison against the previous run, and the reads and the one write
// that turn an extract's output directory into that digest. Every input and the
// output are seams, so a test renders a town it built and writes nothing on the
// host.

// headlineColumns are the town figures the headline table compares: the last
// four weeks against the four before.
var headlineColumns = []string{"size_median", "file_entropy_mean", "rejected_share", "rho", "mode_shift"}

// flaggedColumns are the columns MovedSinceLast watches: each is a figure whose
// newest week is worth a look when it sits far from its own history.
var flaggedColumns = []string{
	"size_median", "file_entropy_mean", "rejected_share", "attempts_mean", "rho",
	"dependency_density", "h_labels", "handoff_bytes_median", "conflict_gate_share",
}

// countColumns are the weekly columns typed as counts, which print
// without a decimal point. Every other figure prints with three significant
// digits, handoff_bytes_total among them: that one stays a float.
var countColumns = map[string]bool{
	"landings": true, "unreadable": true, "manual_rescue_count": true,
	"rejections": true, "rho_numerator": true, "rho_denominator": true,
}

// metricSection is one "## Metric N" block of the digest: the text is fixed so
// a reader of two runs sees the same headings and caveats.
type metricSection struct {
	title      string
	definition string
	columns    []string
	caveat     string
}

// metricSections are the digest's metric blocks, in the order it publishes
// them.
var metricSections = []metricSection{
	{
		title:      "Landing size",
		definition: "Lines added plus deleted per landing: median and 90th percentile, with landings per week.",
		columns:    []string{"landings", "unreadable", "size_median", "size_p90"},
		caveat:     "Landings with an unreadable commit are counted in `unreadable`, never dropped silently.",
	},
	{
		title:      "File and package entropy",
		definition: "Shannon entropy, in bits, of lines changed across files and across directories, averaged over the week's landings.",
		columns:    []string{"file_entropy_mean", "package_entropy_mean"},
		caveat:     "0 for a single-file landing. A rename counts as one file.",
	},
	{
		title:      "Mode shift",
		definition: "Distance between consecutive weeks of the z-scored median size, file entropy and rework share.",
		columns:    []string{"mode_shift"},
		caveat:     "Thin weeks (fewer than 10 landings) are reported but never flagged.",
	},
	{
		title:      "Rework depth",
		definition: "Attempts per landed bead, share rejected before landing, and share approved first try.",
		columns:    []string{"attempts_mean", "rejected_share", "first_try_share"},
		caveat:     "Beads abandoned before landing are absent; the denominator is landed beads.",
	},
	{
		title:      "Label entropy",
		definition: "Entropy, in bits, of the labels, rigs and issue types of beads created that week.",
		columns:    []string{"h_labels", "h_rigs", "h_types"},
		caveat:     "Three separate figures, not blended. Labels with fewer than 3 uses fold into other.",
	},
	{
		title:      "Handoff size",
		definition: "Bytes of notes plus comments on handoff-labelled beads closed that week, and sessions per bead.", //nolint:misspell // the text the report publishes, kept verbatim
		columns:    []string{"handoff_bytes_total", "handoff_bytes_median", "sessions_median"},
		caveat:     "Includes beads in `~/.claude`.",
	},
	{
		title:      "rho",
		definition: "(Non-daemon landings plus crew or handoff beads closed) divided by daemon-dispatched landings.",
		columns:    []string{"rho_numerator", "rho_denominator", "rho"},
		caveat:     "Numerator and denominator are both published. The proxy for human-planned work is crew and handoff beads.",
	},
	{
		title:      "Dependency density",
		definition: "Mean outgoing dependency count of beads closed that week.",
		columns:    []string{"dependency_density"},
		caveat:     "Outgoing edges per bead; `bd list` gives counts, not edges.",
	},
	{
		title:      "Non-conservative merges",
		definition: "Rejections of kind conflict or gate as a share of all rejections, and the count of landings that were not daemon-routed.",
		columns:    []string{"rejections", "conflict_gate_share", "unknown_share", "manual_rescue_count"},
		caveat:     "Kinds come from the kind word in the bead's rejection notes; an unrecognised word counts as `unknown`. Only beads that eventually landed are counted, in the week they landed.", //nolint:misspell // the text the report publishes, kept verbatim
	},
}

// ReportRow is one week as the digest reads it: the week, its rig, whether the
// week is thin, and every column's value with nil where the week has none.
//
// A count is a value here too, so a row can lack one and the digest prints n/a:
// a row with an absent count carries nil, where WeeklyMetrics
// keeps an int (load_result.go).
type ReportRow struct {
	Week   string
	Rig    string
	Thin   bool
	Values map[string]*float64
}

// MovedFlag is one column of one rig's newest week that sits more than two
// standard deviations from its own history: a prompt to look, not a verdict.
type MovedFlag struct {
	Rig, Week, Column string
	Value, Mean, SD   float64
}

// formatFigure formats a figure: "n/a" when the week has none, else
// three significant digits, and nan, inf or -inf for a non-finite value.
func formatFigure(v *float64) string {
	if v == nil {
		return "n/a"
	}
	switch {
	case math.IsNaN(*v):
		return "nan"
	case math.IsInf(*v, 1):
		return "inf"
	case math.IsInf(*v, -1):
		return "-inf"
	}
	return strconv.FormatFloat(*v, 'g', 3, 64)
}

// formatCount is render._fmt for a count: "n/a" when the week has none, else the
// count without a decimal point.
func formatCount(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return strconv.FormatFloat(*v, 'f', 0, 64)
}

// formatNumber is formatFigure for a value the caller holds, for the parts of a
// flag line.
func formatNumber(v float64) string {
	return formatFigure(&v)
}

// reportCell is one cell of a report table: the row's value for column as
// the digest prints it. A column the row does not carry is an error.
func reportCell(r ReportRow, column string) (string, error) {
	v, ok := r.Values[column]
	if !ok {
		return "", fmt.Errorf("report: the %s %s row has no %q column", r.Rig, r.Week, column)
	}
	if countColumns[column] {
		return formatCount(v), nil
	}
	return formatFigure(v), nil
}

// meanOf is render._mean: the mean of the rows' values for column, with the
// weeks that have none left out, and nil when no week has one.
func meanOf(rows []ReportRow, column string) (*float64, error) {
	var values []float64
	for _, r := range rows {
		v, ok := r.Values[column]
		if !ok {
			return nil, fmt.Errorf("report: the %s %s row has no %q column", r.Rig, r.Week, column)
		}
		if v != nil {
			values = append(values, *v)
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	mean := fmean(values)
	return &mean, nil
}

// table is render._table: a Markdown table with header and separator first.
func table(header []string, rows [][]string) string {
	lines := make([]string, 0, len(rows)+2)
	lines = append(lines, "| "+strings.Join(header, " | ")+" |", "|"+strings.Repeat("---|", len(header)))
	for _, row := range rows {
		lines = append(lines, "| "+strings.Join(row, " | ")+" |")
	}
	return strings.Join(lines, "\n")
}

// MovedSinceLast is render.moved_since_last: it flags each column of each rig's
// newest, non-thin week that is more than two population standard deviations
// from the same rig's history in prev, which needs at least six non-thin weeks
// carrying a value for that column. A column the rows do not carry is absent
// from every week, and so never flags.
func MovedSinceLast(prev, cur []ReportRow, columns []string) []MovedFlag {
	var flags []MovedFlag
	for _, rig := range rigNames(cur) {
		newest := ""
		for _, r := range cur {
			if r.Rig == rig && r.Week > newest {
				newest = r.Week
			}
		}
		var latest *ReportRow
		for i := range cur {
			if cur[i].Rig == rig && cur[i].Week == newest {
				latest = &cur[i]
				break
			}
		}
		if latest == nil || latest.Thin {
			continue
		}
		for _, column := range columns {
			var history []float64
			for _, r := range prev {
				if r.Rig != rig || r.Thin {
					continue
				}
				if v := r.Values[column]; v != nil {
					history = append(history, *v)
				}
			}
			if len(history) < 6 {
				continue
			}
			value := latest.Values[column]
			if value == nil {
				continue
			}
			mean, sd := fmean(history), pstdev(history)
			if sd > 0 && math.Abs(*value-mean) > 2*sd {
				flags = append(flags, MovedFlag{
					Rig: rig, Week: newest, Column: column,
					Value: *value, Mean: mean, SD: sd,
				})
			}
		}
	}
	return flags
}

// rigNames is the rigs cur names, sorted, for a comparison that must flag the
// same rigs in the same order on every run.
func rigNames(rows []ReportRow) []string {
	seen := make(map[string]bool, len(rows))
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		if !seen[r.Rig] {
			seen[r.Rig] = true
			names = append(names, r.Rig)
		}
	}
	sort.Strings(names)
	return names
}

// RenderReport is render.render_report: the Markdown digest of one run, from
// the weekly rows (the town's own rig ALL rows are the tables), the flags, the
// footer that says what the figures do not cover, and the two section texts.
func RenderReport(date string, weekly []ReportRow, flags []MovedFlag, footer Footer,
	mangoSection string, regimeEvents []RegimeEvent, backtest string) (string, error) {
	var town []ReportRow
	for _, r := range weekly {
		if r.Rig == "ALL" {
			town = append(town, r)
		}
	}
	sort.SliceStable(town, func(i, j int) bool { return town[i].Week < town[j].Week })

	blocks := []string{"# Town metrics " + date}

	last, before := reportWindow(town, -4, len(town)), reportWindow(town, -8, -4)
	head := make([][]string, 0, len(headlineColumns))
	for _, column := range headlineColumns {
		lastMean, err := meanOf(last, column)
		if err != nil {
			return "", err
		}
		beforeMean, err := meanOf(before, column)
		if err != nil {
			return "", err
		}
		head = append(head, []string{column, formatFigure(lastMean), formatFigure(beforeMean)})
	}
	blocks = append(blocks, "## Headline\n\nLast four weeks against the four before (town, all rigs).\n\n"+
		table([]string{"Metric", "Last 4 weeks", "Previous 4 weeks"}, head))

	blocks = append(blocks, "## Moved since last run\n\n"+movedLines(flags))

	for i, section := range metricSections {
		rows := make([][]string, 0, len(town))
		for _, r := range town {
			week := r.Week
			if r.Thin {
				week += " (thin)"
			}
			row := make([]string, 0, len(section.columns)+1)
			row = append(row, week)
			for _, column := range section.columns {
				text, err := reportCell(r, column)
				if err != nil {
					return "", err
				}
				row = append(row, text)
			}
			rows = append(rows, row)
		}
		header := append([]string{"Week"}, section.columns...)
		blocks = append(blocks, fmt.Sprintf("## Metric %d: %s\n\n%s\n\n%s\n\nCaveat: %s",
			i+1, section.title, section.definition, table(header, rows), section.caveat))
	}

	events := make([]string, 0, len(regimeEvents))
	for _, e := range regimeEvents {
		events = append(events, "- "+e.Date+": "+e.What)
	}
	blocks = append(blocks, "## Regime events\n\n"+strings.Join(events, "\n"))
	blocks = append(blocks, "## Usefulness backtest\n\n"+backtest)
	blocks = append(blocks, "## Mango findings\n\n"+mangoSection)

	quality, err := dataQuality(footer)
	if err != nil {
		return "", err
	}
	blocks = append(blocks, quality)
	return strings.Join(blocks, "\n\n") + "\n", nil
}

// reportWindow is rows[from:to] with negative indices counted from the end,
// so the digest's headline takes the last four weeks and the four before them, and an empty
// window where the run has fewer weeks.
func reportWindow(rows []ReportRow, from, to int) []ReportRow {
	n := len(rows)
	if from < 0 {
		from = max(0, n+from)
	}
	if to < 0 {
		to = max(0, n+to)
	}
	to = min(to, n)
	if from >= to {
		return nil
	}
	return rows[from:to]
}

// movedLines is the "Moved since last run" block's body.
func movedLines(flags []MovedFlag) string {
	if len(flags) == 0 {
		return "Nothing moved more than two standard deviations."
	}
	lines := make([]string, 0, len(flags))
	for _, f := range flags {
		lines = append(lines, fmt.Sprintf("- %s %s %s: %s (history mean %s, sd %s)",
			f.Rig, f.Week, f.Column, formatNumber(f.Value), formatNumber(f.Mean), formatNumber(f.SD)))
	}
	return strings.Join(lines, "\n")
}

// dataQuality is the digest's closing block: the rows each source gave, the
// rows and gaps it refused, and the sources it could not read at all.
func dataQuality(footer Footer) (string, error) {
	keys := make([]string, 0, len(footer.RowsRead))
	for key := range footer.RowsRead {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	read := make([]string, 0, len(keys))
	for _, key := range keys {
		read = append(read, fmt.Sprintf("- %s: %d", key, footer.RowsRead[key]))
	}
	readBlock := "Rows read: none"
	if len(read) > 0 {
		readBlock = "Rows read:\n" + strings.Join(read, "\n")
	}

	dropped, err := footerEntries(footer.RowsDropped)
	if err != nil {
		return "", err
	}
	gaps, err := footerEntries(footer.Gaps)
	if err != nil {
		return "", err
	}
	sources := "none"
	if len(footer.SourcesUnreadable) > 0 {
		sources = strings.Join(footer.SourcesUnreadable, ", ")
	}
	return strings.Join([]string{
		"## Data quality",
		readBlock,
		fmt.Sprintf("Rows dropped: %d", len(footer.RowsDropped)) + dropped,
		fmt.Sprintf("Other gaps: %d", len(footer.Gaps)) + gaps,
		"Sources unreadable: " + sources,
	}, "\n\n"), nil
}

// footerEntries formats a footer's (source, reason) pairs as list items, one
// per line, and refuses a pair that is not exactly those two fields.
func footerEntries(pairs [][]string) (string, error) {
	var out strings.Builder
	for _, pair := range pairs {
		if len(pair) != 2 {
			return "", fmt.Errorf("report: footer entry %v has %d fields, not 2", pair, len(pair))
		}
		out.WriteString("\n- " + pair[0] + ": " + pair[1])
	}
	return out.String(), nil
}

// reportRows is []WeeklyMetrics as the digest reads it: every column the digest
// can print, with the counts kept as whole figures.
func reportRows(weekly []WeeklyMetrics) []ReportRow {
	out := make([]ReportRow, 0, len(weekly))
	for _, m := range weekly {
		r := ReportRow{Week: m.Week, Rig: m.Rig, Thin: m.Thin, Values: make(map[string]*float64, len(WeeklyColumns))}
		for _, column := range WeeklyColumns {
			switch column {
			case "week", "rig", "thin", "dominant_model":
				continue
			}
			if v, ok := weeklyFigure(m, column); ok {
				r.Values[column] = v
				continue
			}
			count, _ := weeklyCount(m, column)
			value := float64(count)
			r.Values[column] = &value
		}
		out = append(out, r)
	}
	return out
}

// weeklyFigure returns one weekly row's figure for column, nil when the week
// has none; the second result is false for a column that is a count.
func weeklyFigure(m WeeklyMetrics, column string) (*float64, bool) {
	switch column {
	case "size_median":
		return m.SizeMedian, true
	case "size_p90":
		return m.SizeP90, true
	case "file_entropy_mean":
		return m.FileEntropyMean, true
	case "package_entropy_mean":
		return m.PackageEntropyMean, true
	case "attempts_mean":
		return m.AttemptsMean, true
	case "rejected_share":
		return m.RejectedShare, true
	case "first_try_share":
		return m.FirstTryShare, true
	case "h_labels":
		return m.HLabels, true
	case "h_rigs":
		return m.HRigs, true
	case "h_types":
		return m.HTypes, true
	case "handoff_bytes_median":
		return m.HandoffBytesMedian, true
	case "sessions_median":
		return m.SessionsMedian, true
	case "rho":
		return m.Rho, true
	case "dependency_density":
		return m.DependencyDensity, true
	case "conflict_gate_share":
		return m.ConflictGateShare, true
	case "unknown_share":
		return m.UnknownShare, true
	case "mode_shift":
		return m.ModeShift, true
	case "om_threshold":
		return m.OMThreshold, true
	}
	return nil, false
}

// weeklyCount returns one weekly row's count for column; the second result is
// false for a column that is a figure.
func weeklyCount(m WeeklyMetrics, column string) (int, bool) {
	switch column {
	case "landings":
		return m.Landings, true
	case "unreadable":
		return m.Unreadable, true
	case "manual_rescue_count":
		return m.ManualRescueCount, true
	case "handoff_bytes_total":
		return m.HandoffBytesTotal, true
	case "rho_numerator":
		return m.RhoNumerator, true
	case "rho_denominator":
		return m.RhoDenominator, true
	case "rejections":
		return m.Rejections, true
	}
	return 0, false
}

// ReportInput is what Report renders and where its digest goes: an earlier
// extract's output directory, the day being reported, and its rows.
type ReportInput struct {
	// OutDir is the extract's output directory: where weekly.csv, the mango and
	// backtest sections are read and where the earlier runs sit beside it.
	OutDir string
	// DocsDir is where the digest and regime-events.json live.
	DocsDir string
	// Today is the day the digest is named for and dated.
	Today string
	// Weekly and Footer are the extract's result.
	Weekly []WeeklyMetrics
	Footer Footer
	// MangoSection is the section text when the caller holds one; nil reads
	// <OutDir>/mango-section.md and says "Not run." when there is none.
	MangoSection *string
	// MkdirAll makes DocsDir; nil makes it on the host.
	MkdirAll func(dir string) error
	// WriteFile writes the digest; nil writes it on the host.
	WriteFile func(path string, data []byte) error
}

const (
	reportMangoName    = "mango-section.md"
	reportBacktestName = "backtest-section.md"
	reportNotRun       = "Not run."
)

// Report is cli.report: it writes <DocsDir>/<Today>-town-metrics.md from the
// extract's result and returns the path it wrote.
func Report(env Env, in ReportInput) (string, error) {
	switch {
	case in.OutDir == "":
		return "", errors.New("report: ReportInput.OutDir is unset")
	case in.DocsDir == "":
		return "", errors.New("report: ReportInput.DocsDir is unset")
	case in.Today == "":
		return "", errors.New("report: ReportInput.Today is unset")
	case env.ReadFile == nil:
		return "", errors.New("report: Env.ReadFile is unset")
	}

	mangoSection := reportNotRun
	if in.MangoSection != nil {
		mangoSection = *in.MangoSection
	} else {
		data, err := readIfPresent(env, filepath.Join(in.OutDir, reportMangoName))
		if err != nil {
			return "", err
		}
		if data != nil {
			mangoSection = strings.TrimSpace(string(data))
		}
	}

	backtest := reportNotRun
	data, err := readIfPresent(env, filepath.Join(in.OutDir, reportBacktestName))
	if err != nil {
		return "", err
	}
	if data != nil {
		backtest = strings.TrimSpace(string(data))
	}

	previous, err := PreviousWeekly(env, in.OutDir)
	if err != nil {
		return "", err
	}
	weekly := reportRows(in.Weekly)
	flags := MovedSinceLast(reportRows(previous), weekly, flaggedColumns)

	var events []RegimeEvent
	data, err = readIfPresent(env, filepath.Join(in.DocsDir, "regime-events.json"))
	if err != nil {
		return "", err
	}
	if data != nil {
		if err := json.Unmarshal(data, &events); err != nil {
			return "", fmt.Errorf("report: regime-events.json: %w", err)
		}
	}

	text, err := RenderReport(in.Today, weekly, flags, in.Footer, mangoSection, events, backtest)
	if err != nil {
		return "", err
	}
	if err := in.mkdirAll(in.DocsDir); err != nil {
		return "", err
	}
	path := filepath.Join(in.DocsDir, in.Today+"-town-metrics.md")
	if err := in.writeFile(path, []byte(text)); err != nil {
		return "", fmt.Errorf("report: %s: %w", path, err)
	}
	return path, nil
}

// mkdirAll makes the digest's directory through the caller's seam.
func (in ReportInput) mkdirAll(dir string) error {
	if in.MkdirAll != nil {
		return in.MkdirAll(dir)
	}
	return os.MkdirAll(dir, 0o755)
}

// writeFile writes the digest through the caller's seam.
func (in ReportInput) writeFile(path string, data []byte) error {
	if in.WriteFile != nil {
		return in.WriteFile(path, data)
	}
	return os.WriteFile(path, data, 0o644)
}

// readIfPresent reads path through env.ReadFile and reports a missing file as
// nil, nil; any other failure is returned, since an unreadable section must not
// read as an absent one.
func readIfPresent(env Env, path string) ([]byte, error) {
	data, err := env.ReadFile(path)
	if err == nil {
		return data, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return nil, fmt.Errorf("report: %s: %w", path, err)
}

// PreviousWeekly is cli.previous_weekly: the newest earlier run's weekly.csv
// beside outDir (same parent, smaller name), typed, and nothing when there is
// none.
func PreviousWeekly(env Env, outDir string) ([]WeeklyMetrics, error) {
	if env.ReadFile == nil {
		return nil, errors.New("previous weekly: Env.ReadFile is unset")
	}
	if env.ReadDir == nil {
		return nil, errors.New("previous weekly: Env.ReadDir is unset")
	}
	parent, mine := filepath.Split(filepath.Clean(outDir))
	dir := filepath.Clean(parent)
	entries, err := env.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("previous weekly: %s: %w", dir, err)
	}

	var earlier []string
	for _, entry := range entries {
		if entry.IsDir && entry.Name < mine {
			earlier = append(earlier, entry.Name)
		}
	}
	sort.Strings(earlier)
	for i := len(earlier) - 1; i >= 0; i-- {
		path := filepath.Join(parent, earlier[i], weeklyName)
		data, err := env.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("previous weekly: %s: %w", path, err)
		}
		rows, err := ReadCSV(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("previous weekly: %s: %w", path, err)
		}
		weekly, err := TypedWeekly(rows)
		if err != nil {
			return nil, fmt.Errorf("previous weekly: %s: %w", path, err)
		}
		return weekly, nil
	}
	return nil, nil
}

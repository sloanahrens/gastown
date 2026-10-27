package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// This file ports ExtractEnv and the extract orchestration of
// scripts/town_metrics/cli.py: the injected sources, the row-count assertion
// that refuses a loader which lost a row silently, and the join that writes
// landings.csv, its sidecar, weekly.csv, beads.csv and footer.json.
//
// Every source arrives by injection, as in the Python, so a unit test reads a
// fake town and never ~/gt, bd or git.

// beadsName is the bead listing the extract writes beside its tables.
const beadsName = "beads.csv"

// beadColumns is the header beads.csv carries: BEAD_COLUMNS in cli.py, in its
// order.
var beadColumns = []string{
	"id", "rig", "created_at", "closed_at", "type", "labels", "dependency_count",
	"comment_count", "handoff_bytes", "sessions",
}

// defaultBdShowCap is how many beads the handoff reader may fetch when the env
// names no cap: the Python Env's own default.
const defaultBdShowCap = 150

// RowCountError is the extract's refusal when a loader returns fewer rows than
// its source lines account for: a refused line must be logged, and the extract
// stops rather than publish a table that silently lost a row.
type RowCountError struct {
	Source string
	Rows   int
	Drops  int
	Lines  int
}

func (e *RowCountError) Error() string {
	return fmt.Sprintf("%s: %d rows + %d logged drops != %d source lines", e.Source, e.Rows, e.Drops, e.Lines)
}

// ExtractEnv is everything the extract reads and where it writes: the Python
// Env's injected sources under the same names, so one test drives the whole
// pipeline from fakes.
type ExtractEnv struct {
	// LedgerLines is each rig's ledger.jsonl, still as lines: a line the loader
	// refuses is logged, and the row-count assertion counts it.
	LedgerLines map[string][]string
	// ReviewLines is reviews.jsonl, still as lines.
	ReviewLines []string
	// Diffs is where a landing's size and spread come from.
	Diffs DiffSource
	// RunBd runs bd in a directory and returns its stdout.
	RunBd BdRun
	// BeadDirs is every directory bd is listed in: each rig's and the town's.
	BeadDirs []string
	// PrefixToRig maps a bead id's prefix to its rig name.
	PrefixToRig map[string]string
	// UsageFiles is each bead's parsed *.work.json.
	UsageFiles map[string]UsageFile
	// OverrunStates is each bead's parsed overrun-guard state file.
	OverrunStates map[string]OverrunState
	// OMCommits is each rig's .om.json history, oldest first.
	OMCommits map[string][]OMCommit
	// OutDir is the directory the extract writes its files to.
	OutDir string
	// DocsDir is where the report publishes. The extract writes nothing there;
	// the field is carried so one env serves both stages, as the Python's does.
	DocsDir string
	// Today is the day being reported.
	Today string
	// Unreadable lists the sources the wiring could not open at all.
	Unreadable []string
	// HandoffCache is the bead-to-bytes map the wiring loaded; the extract
	// fills it as it fetches, so the caller can save it back.
	HandoffCache map[string]int
	// BdShowCap bounds how many beads the handoff reader fetches; zero is the
	// Python Env's default of 150.
	BdShowCap int
	// LedgerLoader and ReviewLoader are the loaders the row-count assertion
	// watches; nil means LoadLedger and LoadReviews. A test injects a loader
	// that loses a row to drive the assertion, as the Python's test does.
	LedgerLoader func(map[string][]string, *Dropped) []LedgerRow
	ReviewLoader func([]string, *Dropped) []Review
	// Create opens one output file by name, for a test that records the writes
	// and the order it made them in; nil writes to the host filesystem under
	// OutDir.
	Create func(name string) (io.WriteCloser, error)
	// MkdirAll makes OutDir before the first write; nil is the host's.
	MkdirAll func(dir string) error
}

// ExtractResult is what an extract produced: the rows it wrote, for a later
// report, and the footer that says what those rows do not cover.
type ExtractResult struct {
	Landings []Row
	Weekly   []WeeklyMetrics
	Footer   Footer
}

// Extract joins the injected sources into the extract's files, as cli.py's
// extract does. It refuses (RowCountError) a loader that returns fewer rows
// than its source lines account for, and a source it cannot read is logged in
// dropped rather than read as an empty one.
//
// The files go out in the Python's order: landings.csv, its sidecar, weekly.csv,
// beads.csv (left out when no bd list worked, so an empty listing is never
// published as a real answer) and footer.json last, describing the rest.
func Extract(ctx context.Context, env ExtractEnv) (ExtractResult, error) {
	var res ExtractResult
	switch {
	case env.OutDir == "":
		return res, errors.New("extract: ExtractEnv.OutDir is unset")
	case env.Diffs == nil:
		return res, errors.New("extract: ExtractEnv.Diffs is unset")
	case env.RunBd == nil:
		return res, errors.New("extract: ExtractEnv.RunBd is unset")
	}
	if err := env.makeOutDir(); err != nil {
		return res, fmt.Errorf("extract: %w", err)
	}
	dropped := &Dropped{}

	ledgerLoader := env.LedgerLoader
	if ledgerLoader == nil {
		ledgerLoader = LoadLedger
	}
	ledger := ledgerLoader(env.LedgerLines, dropped)
	ledgerLines := 0
	for _, lines := range env.LedgerLines {
		ledgerLines += nonblankLines(lines)
	}
	ledgerDrops := countDroppedByPrefix(dropped, "ledger:")
	if len(ledger)+ledgerDrops != ledgerLines {
		return res, &RowCountError{Source: "ledger", Rows: len(ledger), Drops: ledgerDrops, Lines: ledgerLines}
	}

	reviewLoader := env.ReviewLoader
	if reviewLoader == nil {
		reviewLoader = LoadReviews
	}
	reviews := reviewLoader(env.ReviewLines, dropped)
	reviewLines := nonblankLines(env.ReviewLines)
	reviewDrops := countDroppedBySource(dropped, "reviews")
	if len(reviews)+reviewDrops != reviewLines {
		return res, &RowCountError{Source: "reviews", Rows: len(reviews), Drops: reviewDrops, Lines: reviewLines}
	}

	beads := LoadBeads(env.RunBd, env.BeadDirs, dropped)
	for i := range beads {
		// The loader attributes a bead to its id's prefix; every table carries
		// the rig's name instead.
		if rig, ok := env.PrefixToRig[beads[i].Rig]; ok {
			beads[i].Rig = rig
		}
	}
	unreadable := make([]string, 0, len(env.Unreadable)+len(dropped.Failed))
	unreadable = append(unreadable, env.Unreadable...)
	unreadable = append(unreadable, dropped.Failed...)
	// Every bd list failing is the one case that leaves beads.csv out: absence
	// is the signal, where an empty table would claim the town has no beads.
	// Only a listing that failed counts, not a row the handoff reader refused
	// later, so a loaded-but-incomplete listing still writes its table.
	bdListsOK := countFailedByPrefix(dropped, "bd:") < len(env.BeadDirs)

	landedAt := make(map[string]string, len(ledger))
	for _, l := range ledger {
		landedAt[l.Bead] = l.LandedAt
	}
	noteKinds := make(map[string][]string)
	for _, b := range beads {
		if _, landed := landedAt[b.ID]; landed && len(b.RejectionKinds) > 0 {
			noteKinds[b.ID] = b.RejectionKinds
		}
	}

	rework, err := ReworkByBead(reviews, landedAt, noteKinds)
	if err != nil {
		return res, err
	}
	usage := LoadUsage(env.UsageFiles)
	ledgerBeads := make([]string, 0, len(landedAt))
	for bead := range landedAt {
		ledgerBeads = append(ledgerBeads, bead)
	}
	sort.Strings(ledgerBeads)
	overrun := LoadOverrun(env.OverrunStates, ledgerBeads)

	// The rigs' histories are read in sorted order so a broken threshold names
	// the same rig on every run: a map has no order to be stable with.
	eras := make(map[string][]OMEra, len(env.OMCommits))
	omRigs := make([]string, 0, len(env.OMCommits))
	for rig := range env.OMCommits {
		omRigs = append(omRigs, rig)
	}
	sort.Strings(omRigs)
	for _, rig := range omRigs {
		rigEras, err := OMEras(env.OMCommits[rig])
		if err != nil {
			return res, err
		}
		eras[rig] = rigEras
	}

	beadsByID := make(map[string]BeadRow, len(beads))
	shaped := make(map[string]bool)
	for _, b := range beads {
		beadsByID[b.ID] = b
		if b.Shaped {
			shaped[b.ID] = true
		}
	}

	landings, err := BuildLandings(ctx, ledger, rework, env.Diffs, beadsByID, usage, overrun, eras, shaped, dropped)
	if err != nil {
		return res, err
	}

	handoffDirs := make(map[string]string)
	for _, b := range beads {
		if slices.Contains(b.Labels, "handoff") {
			handoffDirs[b.ID] = b.Dir
		}
	}
	callCap := env.BdShowCap
	if callCap == 0 {
		callCap = defaultBdShowCap
	}
	handoff := HandoffBytes(env.RunBd, handoffDirs, dropped, callCap, env.HandoffCache)
	sessions := make(map[string]int, len(usage))
	for bead, u := range usage {
		sessions[bead] = u.Sessions
	}

	kinds := make(map[WeekRig][]string)
	for _, l := range landings {
		for _, kind := range noteKinds[l["bead"]] {
			for _, rig := range []string{l["rig"], "ALL"} {
				key := WeekRig{Week: l["week"], Rig: rig}
				kinds[key] = append(kinds[key], ClassifyRejection(kind))
			}
		}
	}

	// A label that came up three times is common enough to summarize; the
	// rarer ones are counted one by one, so the entropies are not swamped by
	// labels that name a single bead.
	uses := make(map[string]int)
	for _, b := range beads {
		seen := make(map[string]bool, len(b.Labels))
		for _, label := range b.Labels {
			if seen[label] {
				continue
			}
			seen[label] = true
			uses[label]++
		}
	}
	rare := make(map[string]bool)
	for label, n := range uses {
		if n < 3 {
			rare[label] = true
		}
	}

	weekly, err := BuildWeekly(landings, weeklyBeads(beads), handoff, sessions, kinds, rare)
	if err != nil {
		return res, err
	}

	if err := env.writeOutput(landingsName, func(w io.Writer) error {
		return WriteCSV(w, landings, landingColumns)
	}); err != nil {
		return res, err
	}
	if err := env.writeOutput(sidecarName, WriteLandingSidecar); err != nil {
		return res, err
	}
	weeklyRows := weeklyCSVRows(weekly)
	if err := env.writeOutput(weeklyName, func(w io.Writer) error {
		return WriteCSV(w, weeklyRows, WeeklyColumns)
	}); err != nil {
		return res, err
	}
	if bdListsOK {
		beadRows := beadsCSVRows(beads, handoff, usage)
		if err := env.writeOutput(beadsName, func(w io.Writer) error {
			return WriteCSV(w, beadRows, beadColumns)
		}); err != nil {
			return res, err
		}
	}

	rowsDropped := make([][]string, 0)
	gaps := make([][]string, 0)
	for _, item := range dropped.Items {
		if strings.HasPrefix(item.Source, "ledger:") || item.Source == "reviews" {
			rowsDropped = append(rowsDropped, []string{item.Source, item.Reason})
			continue
		}
		gaps = append(gaps, []string{item.Source, item.Reason})
	}
	footer := Footer{
		RowsRead: map[string]int{
			"ledger":  ledgerLines,
			"reviews": reviewLines,
			"beads":   len(beads),
			"usage":   len(env.UsageFiles),
		},
		RowsDropped:       rowsDropped,
		Gaps:              gaps,
		SourcesUnreadable: unreadable,
	}
	if err := env.writeOutput(footerName, func(w io.Writer) error {
		return writeFooter(w, footer)
	}); err != nil {
		return res, err
	}
	return ExtractResult{Landings: landings, Weekly: weekly, Footer: footer}, nil
}

// makeOutDir makes the output directory, as the Python's os.makedirs does
// before the first write.
func (env ExtractEnv) makeOutDir() error {
	if env.MkdirAll != nil {
		return env.MkdirAll(env.OutDir)
	}
	return os.MkdirAll(env.OutDir, 0o755)
}

// openOutput opens one of the extract's output files by name.
func (env ExtractEnv) openOutput(name string) (io.WriteCloser, error) {
	if env.Create != nil {
		return env.Create(name)
	}
	return os.Create(filepath.Join(env.OutDir, name))
}

// writeOutput writes one output file through the env's opener, naming the file
// on a failed open, write or close, so a partial file is never read as a
// finished one.
func (env ExtractEnv) writeOutput(name string, write func(io.Writer) error) error {
	f, err := env.openOutput(name)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := write(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// nonblankLines counts the lines a source really carries: a blank line is no
// row, so it is neither a row nor a drop.
func nonblankLines(lines []string) int {
	n := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// countDroppedByPrefix counts the log's entries whose source starts with
// prefix, which is how the ledger's drops are told apart from every rig's.
func countDroppedByPrefix(d *Dropped, prefix string) int {
	n := 0
	for _, item := range d.Items {
		if strings.HasPrefix(item.Source, prefix) {
			n++
		}
	}
	return n
}

// countFailedByPrefix counts the log's unreadable sources whose name starts
// with prefix: a listing that failed is not the same as a row it refused.
func countFailedByPrefix(d *Dropped, prefix string) int {
	n := 0
	for _, source := range d.Failed {
		if strings.HasPrefix(source, prefix) {
			n++
		}
	}
	return n
}

// countDroppedBySource counts the log's entries from exactly one source.
func countDroppedBySource(d *Dropped, source string) int {
	n := 0
	for _, item := range d.Items {
		if item.Source == source {
			n++
		}
	}
	return n
}

// weeklyBeads is the weekly metrics' view of the bead listing: a bead no
// closed_at field carries is an empty timestamp, which is the Python's None.
func weeklyBeads(beads []BeadRow) []Bead {
	out := make([]Bead, 0, len(beads))
	for _, b := range beads {
		var closed string
		if b.ClosedAt != nil {
			closed = *b.ClosedAt
		}
		out = append(out, Bead{
			ID:              b.ID,
			Rig:             b.Rig,
			CreatedAt:       b.CreatedAt,
			ClosedAt:        closed,
			Type:            b.Type,
			Labels:          b.Labels,
			DependencyCount: b.DependencyCount,
		})
	}
	return out
}

// weeklyCSVRows is weekly.csv's rows: the typed weekly metrics through Cell,
// which is what the report reads back with TypedWeekly. Every column of
// WeeklyColumns has a cell, which TestBuildWeeklyRowsCarryEveryColumn holds the
// header to, so a row fills the header it is written under.
func weeklyCSVRows(rows []WeeklyMetrics) []Row {
	out := make([]Row, 0, len(rows))
	for _, m := range rows {
		row := make(Row, len(WeeklyColumns))
		for _, column := range WeeklyColumns {
			row[column], _ = m.Cell(column)
		}
		out = append(out, row)
	}
	return out
}

// beadsCSVRows is beads.csv's rows: each bead's own fields, the handoff size
// the reader fetched and the sessions its usage file recorded. A bead either
// of those did not cover is an empty cell, never a zero the reader would take
// for a measurement.
func beadsCSVRows(beads []BeadRow, handoff map[string]int, usage map[string]Usage) []Row {
	rows := make([]Row, 0, len(beads))
	for _, b := range beads {
		row := Row{
			"id":               b.ID,
			"rig":              b.Rig,
			"created_at":       b.CreatedAt,
			"type":             b.Type,
			"labels":           strings.Join(b.Labels, ";"),
			"dependency_count": strconv.Itoa(b.DependencyCount),
			"comment_count":    strconv.Itoa(b.CommentCount),
		}
		if b.ClosedAt != nil {
			row["closed_at"] = *b.ClosedAt
		}
		if n, ok := handoff[b.ID]; ok {
			row["handoff_bytes"] = strconv.Itoa(n)
		}
		if u, ok := usage[b.ID]; ok {
			row["sessions"] = strconv.Itoa(u.Sessions)
		}
		rows = append(rows, row)
	}
	return rows
}

// footerDoc is footer.json's shape with its fields in the order the extract
// builds them, so the file reads like the one the Python's json.dump writes.
// The two are not byte-identical: Go spaces the separators its own way, and the
// parity run compares cells, not this file (gt-f3dmx).
type footerDoc struct {
	RowsRead          footerRowsRead `json:"rows_read"`
	RowsDropped       [][]string     `json:"rows_dropped"`
	Gaps              [][]string     `json:"gaps"`
	SourcesUnreadable []string       `json:"sources_unreadable"`
}

// footerRowsRead is footerDoc's rows_read object, in the Python's order.
type footerRowsRead struct {
	Ledger  int `json:"ledger"`
	Reviews int `json:"reviews"`
	Beads   int `json:"beads"`
	Usage   int `json:"usage"`
}

// writeFooter writes footer.json as the extract's footer.
func writeFooter(w io.Writer, f Footer) error {
	return writeJSON(w, footerName, footerDoc{
		RowsRead: footerRowsRead{
			Ledger:  f.RowsRead["ledger"],
			Reviews: f.RowsRead["reviews"],
			Beads:   f.RowsRead["beads"],
			Usage:   f.RowsRead["usage"],
		},
		RowsDropped:       f.RowsDropped,
		Gaps:              f.Gaps,
		SourcesUnreadable: f.SourcesUnreadable,
	})
}

// writeJSON writes one extract file as compact JSON with no trailing newline,
// as the Python's json.dump leaves it, naming the file when the value cannot be
// encoded or the write fails.
func writeJSON(w io.Writer, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

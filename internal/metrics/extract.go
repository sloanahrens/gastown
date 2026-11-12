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

	"github.com/steveyegge/gastown/internal/land"
)

// This file ports ExtractEnv and the extract orchestration of the town metrics
// script: the injected sources, the row-count assertion that refuses a loader
// which lost a row silently, and the join that writes landings.csv, its
// sidecar, weekly.csv, beads.csv and footer.json.
//
// Every source arrives by injection, so a unit test reads a fake town and never
// ~/gt, bd or git.

// beadsName is the bead listing the extract writes beside its tables.
const beadsName = "beads.csv"

// beadColumns is the header beads.csv carries, in the order the extract writes
// it.
var beadColumns = []string{
	"id", "rig", "created_at", "closed_at", "type", "labels", "dependency_count",
	"comment_count", "handoff_bytes", "sessions",
}

// defaultBdShowCap is how many beads the handoff reader may fetch when the env
// names no cap: the ported default.
const defaultBdShowCap = 150

// RowCountError is the extract's refusal when a loader returns fewer rows than
// its source lines account for: a refused line must be logged, and the extract
// stops rather than publish a table that silently lost a row.
type RowCountError struct {
	Source string
	Rows   int
	Drops  int
	// Queue is the lines the source holds that are not rows by design: the
	// reviews log carries the om follow-up queue's entries beside its review
	// lines. They are identified by their marker, so a line that is merely
	// unreadable is still a drop or a refusal, never a queue line.
	Queue int
	Lines int
}

func (e *RowCountError) Error() string {
	if e.Queue > 0 {
		return fmt.Sprintf("%s: %d rows + %d logged drops + %d queue lines != %d source lines", e.Source, e.Rows, e.Drops, e.Queue, e.Lines)
	}
	return fmt.Sprintf("%s: %d rows + %d logged drops != %d source lines", e.Source, e.Rows, e.Drops, e.Lines)
}

// ExtractEnv is everything the extract reads and where it writes: every source
// by injection, so one test drives the whole pipeline from fakes.
type ExtractEnv struct {
	// LedgerLines is each rig's ledger.jsonl, still as lines: a line the loader
	// refuses is logged, and the row-count assertion counts it.
	LedgerLines map[string][]string
	// ReviewLines is reviews.jsonl, still as lines.
	ReviewLines []string
	// EventLines is the town's events feed, still as lines: the dispatch and
	// done clocks the landings join reads. A line the loader refuses is recorded
	// in dropped, and the extract keeps the stamps it could read.
	EventLines []string
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
	// Today is the day being reported.
	Today string
	// Unreadable lists the sources the wiring could not open at all.
	Unreadable []string
	// HandoffCache is the bead-to-bytes map the wiring loaded; the extract
	// fills it as it fetches, so the caller can save it back.
	HandoffCache map[string]int
	// BdShowCap bounds how many beads the handoff reader fetches; zero is the
	// ported default of 150.
	BdShowCap int
	// JevLog is the Jev dispatch log the landings' jev columns read, by path.
	// A missing file leaves those columns empty and is named in the footer; it
	// must be set, so an unwired log is never read as an absent one.
	JevLog string
	// LedgerLoader and ReviewLoader are the loaders the row-count assertion
	// watches; nil means LoadLedger and LoadReviews. A test injects a loader
	// that loses a row to drive the assertion.
	LedgerLoader func(map[string][]string, *Dropped) []LedgerRow
	ReviewLoader func([]string, *Dropped) []Review
	// WeeklyColumns is the header weekly.csv is written under; nil is the
	// package's WeeklyColumns. A test injects a header the metrics cannot fill
	// to drive the missing-cell refusal.
	WeeklyColumns []string
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

// Extract joins the injected sources into the extract's files. It refuses
// (RowCountError) a loader that returns fewer rows than its source lines
// account for, and a source it cannot read is logged in dropped rather than
// read as an empty one.
//
// The files go out in the ported extract's order: landings.csv, its sidecar,
// weekly.csv, beads.csv (left out when no bd list worked, so an empty listing is
// never published as a real answer) and footer.json last, describing the rest.
func Extract(ctx context.Context, env ExtractEnv) (ExtractResult, error) {
	var res ExtractResult
	switch {
	case env.OutDir == "":
		return res, errors.New("extract: ExtractEnv.OutDir is unset")
	case env.Diffs == nil:
		return res, errors.New("extract: ExtractEnv.Diffs is unset")
	case env.RunBd == nil:
		return res, errors.New("extract: ExtractEnv.RunBd is unset")
	case env.JevLog == "":
		// An empty path would read as a missing log, so an unwired one is
		// refused here rather than passed as a log with no answers.
		return res, errors.New("extract: ExtractEnv.JevLog is unset")
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
	// The two loaders run before every other stage, so a drop belongs to one of
	// them if it appeared during its call. The row count is the only guard that
	// notices a row a loader lost without logging it.
	ledgerDrops := dropped.Count()
	if len(ledger)+ledgerDrops != ledgerLines {
		return res, &RowCountError{Source: "ledger", Rows: len(ledger), Drops: ledgerDrops, Lines: ledgerLines}
	}

	reviewLoader := env.ReviewLoader
	if reviewLoader == nil {
		reviewLoader = LoadReviews
	}
	logged := dropped.Count()
	reviews := reviewLoader(env.ReviewLines, dropped)
	reviewLines := nonblankLines(env.ReviewLines)
	reviewDrops := dropped.Count() - logged
	queueLines := queueReviewLines(env.ReviewLines)
	if len(reviews)+reviewDrops+queueLines != reviewLines {
		return res, &RowCountError{Source: "reviews", Rows: len(reviews), Drops: reviewDrops, Queue: queueLines, Lines: reviewLines}
	}
	// Everything logged so far is a refused row; what a later stage logs is a
	// gap in a source that was read.
	rowDrops := dropped.Count()

	failedBefore := len(dropped.Failed)
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
	// later, so a loaded-but-incomplete listing still writes its table. LoadBeads
	// records one failed source per listing it cannot read, so its own additions
	// are the count, whatever names it gives them.
	bdListsOK := len(dropped.Failed)-failedBefore < len(env.BeadDirs)

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
	rounds, err := OMRoundsByBead(reviews, landedAt, dropped)
	if err != nil {
		return res, err
	}
	usage := LoadUsage(env.UsageFiles)
	stamps := LoadStageStamps(env.EventLines, landedAt, dropped)
	stamps.Seats = ReadUsageSeats(env.UsageFiles)
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

	jev, err := LoadJevFirst(env.JevLog, dropped)
	if err != nil {
		return res, err
	}
	landings, err := BuildLandings(ctx, ledger, rework, env.Diffs, beadsByID, usage, overrun, rounds, eras, shaped, stamps, jev, dropped)
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
	weeklyColumns := env.WeeklyColumns
	if weeklyColumns == nil {
		weeklyColumns = WeeklyColumns
	}
	// The rows are built before the first write, so a header no row can fill
	// refuses the extract instead of leaving part of a set behind.
	weeklyRows, err := weeklyCSVRows(weekly, weeklyColumns)
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
	if err := env.writeOutput(weeklyName, func(w io.Writer) error {
		return WriteCSV(w, weeklyRows, weeklyColumns)
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

	rowsDropped := make([][]string, 0, rowDrops)
	gaps := make([][]string, 0, len(dropped.Items)-rowDrops)
	for i, item := range dropped.Items {
		pair := []string{item.Source, item.Reason}
		if i < rowDrops {
			rowsDropped = append(rowsDropped, pair)
			continue
		}
		gaps = append(gaps, pair)
	}
	landingGaps := CountLandingGaps(landings)
	footer := Footer{
		RowsRead: map[string]int{
			"ledger":  ledgerLines,
			"reviews": reviewLines,
			"beads":   len(beads),
			"usage":   len(env.UsageFiles),
		},
		RowsDropped:                   rowsDropped,
		Gaps:                          gaps,
		SourcesUnreadable:             unreadable,
		ModelUnknown:                  landingGaps.ModelUnknown,
		StageStampMissingCreatedAt:    landingGaps.StageStampMissing["created_at"],
		StageStampMissingDispatchedAt: landingGaps.StageStampMissing["dispatched_at"],
		StageStampMissingSubmittedAt:  landingGaps.StageStampMissing["submitted_at"],
	}
	if err := env.writeOutput(footerName, func(w io.Writer) error {
		return writeFooter(w, footer)
	}); err != nil {
		return res, err
	}
	return ExtractResult{Landings: landings, Weekly: weekly, Footer: footer}, nil
}

// makeOutDir makes the output directory before the first write.
func (env ExtractEnv) makeOutDir() error {
	if env.MkdirAll != nil {
		return env.MkdirAll(env.OutDir)
	}
	return os.MkdirAll(env.OutDir, 0o755)
}

// writeOutput writes one output file, naming the file on a failed open, write
// or close, so a partial file is never read as a finished one. The host's
// directory gets the file only once it is whole (writeAtomic); an injected
// Create owns its own file.
func (env ExtractEnv) writeOutput(name string, write func(io.Writer) error) error {
	if env.Create != nil {
		return writeThrough(env.Create, name, write)
	}
	if err := writeAtomic(env.OutDir, name, write); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// writeThrough writes one output file through opener, naming the file on a
// failed open, write or close.
func writeThrough(open func(string) (io.WriteCloser, error), name string, write func(io.Writer) error) error {
	f, err := open(name)
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

// writeAtomic writes dir/name through a temporary file in dir and renames it
// into place only once every byte is written and closed. A failure partway
// therefore leaves nothing under name and no temporary file beside it, and the
// file an earlier run left is untouched rather than truncated.
//
// The temporary file keeps the 0600 mode CreateTemp opens it with: the town's
// own user writes these files and is the one that reads them.
func writeAtomic(dir, name string, write func(io.Writer) error) error {
	f, err := os.CreateTemp(dir, "."+name+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// The remove is a no-op once the rename has taken the name away.
	defer func() { _ = os.Remove(tmp) }()
	if err := writeAndClose(f, write); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return nil
}

// writeAndClose writes one output file and closes it, so the caller renames a
// file whose every byte reached it.
func writeAndClose(f *os.File, write func(io.Writer) error) error {
	if err := write(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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

// queueReviewLines counts the non-blank lines that are om follow-up queue
// entries, which the reviews loader skips without a row or a drop (the same
// land.IsQueueLine test the loader uses), so the row-count assertion can
// account for them as a third kind of line.
func queueReviewLines(lines []string) int {
	n := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" && land.IsQueueLine([]byte(line)) {
			n++
		}
	}
	return n
}

// weeklyBeads is the weekly metrics' view of the bead listing: a bead no
// closed_at field carries is an empty timestamp, which is nil.
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
// which is what the report reads back with TypedWeekly. columns is the header
// the rows are written under; a column WeeklyMetrics cannot fill is an error,
// because the header and Cell have drifted and WriteCSV would otherwise publish
// an empty cell that reads as a measurement.
func weeklyCSVRows(rows []WeeklyMetrics, columns []string) ([]Row, error) {
	out := make([]Row, 0, len(rows))
	for _, m := range rows {
		row := make(Row, len(columns))
		for _, column := range columns {
			cell, ok := m.Cell(column)
			if !ok {
				return nil, fmt.Errorf("weekly.csv: column %q has no cell", column)
			}
			row[column] = cell
		}
		out = append(out, row)
	}
	return out, nil
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
// builds them. The file is not byte-identical to the one a JSON encoder with
// spaced separators writes, and Go writes none, so the parity run compares
// cells and leaves this file alone (gt-f3dmx).
type footerDoc struct {
	RowsRead                      footerRowsRead `json:"rows_read"`
	RowsDropped                   [][]string     `json:"rows_dropped"`
	Gaps                          [][]string     `json:"gaps"`
	SourcesUnreadable             []string       `json:"sources_unreadable"`
	ModelUnknown                  int            `json:"model_unknown"`
	StageStampMissingCreatedAt    int            `json:"stage_stamp_missing_created_at"`
	StageStampMissingDispatchedAt int            `json:"stage_stamp_missing_dispatched_at"`
	StageStampMissingSubmittedAt  int            `json:"stage_stamp_missing_submitted_at"`
}

// footerRowsRead is footerDoc's rows_read object, with its keys in file order.
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
		RowsDropped:                   f.RowsDropped,
		Gaps:                          f.Gaps,
		SourcesUnreadable:             f.SourcesUnreadable,
		ModelUnknown:                  f.ModelUnknown,
		StageStampMissingCreatedAt:    f.StageStampMissingCreatedAt,
		StageStampMissingDispatchedAt: f.StageStampMissingDispatchedAt,
		StageStampMissingSubmittedAt:  f.StageStampMissingSubmittedAt,
	})
}

// writeJSON writes one extract file as compact JSON with no trailing newline,
// as the ported script's json.dump leaves it, naming the file when the value
// cannot be encoded or the write fails.
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

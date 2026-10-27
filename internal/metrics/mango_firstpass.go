package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/land"
)

// The digest's first-pass section: how many of the beads each kind of author
// merged passed their first om review, and how many reviews a merge took. It is
// the view the polecat context fix is read against, so it counts the towns'
// merged beads by the author kind the review data already carries rather than
// deriving a kind of its own (gt-b9j3o.15).

// firstPassKinds are the kinds the table prints, in its row order. The
// overseer's work is counted beside the table, not in it: the view is the
// polecat context fix's, which names the seat kinds it moves.
var firstPassKinds = []string{AuthorKindPolecat, AuthorKindCrew, AuthorKindUnknown}

// MergedBead is one bead a rig's landing log records as landed.
type MergedBead struct {
	// Rig is the rig whose landing log carries the record.
	Rig string
	// Bead is the bead that merged.
	Bead string
}

// FirstPassKindRow is one author kind's line of the first-pass table: how many
// merged beads are the kind's, how many of those were a first pass, and how
// many reviews those beads took.
type FirstPassKindRow struct {
	Kind      string
	Beads     int
	FirstPass int
	Reviews   int
}

// FirstPassTable is the first-pass view's counts.
type FirstPassTable struct {
	// Rows are firstPassKinds in order, each present even at zero, so a kind
	// that merged nothing reads as no beads rather than as a missing row.
	Rows []FirstPassKindRow
	// OtherKinds are the merged beads of a kind the table does not print.
	OtherKinds []FirstPassKindRow
	// Unreviewed is the merged beads the review log carries no line for. They
	// count in no row: without a review line the author kind is not derived,
	// and a bead with no review cannot be read as a failed first pass.
	Unreviewed int
}

// CountFirstPass counts the merged beads by the derived author kind of their
// first review: the first line the review log carries for that bead, since the
// log is append-only and the first verdict it holds is the one the landing got
// first. A bead is a first pass when that verdict is an approve the landing did
// not turn away.
func CountFirstPass(merged []MergedBead, reviews []AttributedRecord) FirstPassTable {
	type key struct{ Rig, Bead string }
	first := make(map[key]AttributedRecord, len(merged))
	lines := make(map[key]int, len(merged))
	for _, review := range reviews {
		k := key{Rig: review.Record.Rig, Bead: review.Record.BeadID}
		if _, ok := first[k]; !ok {
			first[k] = review
		}
		lines[k]++
	}
	rows := make([]FirstPassKindRow, len(firstPassKinds))
	named := make(map[string]int, len(firstPassKinds))
	for i, kind := range firstPassKinds {
		rows[i] = FirstPassKindRow{Kind: kind}
		named[kind] = i
	}
	other := map[string]int{}
	var table FirstPassTable
	seen := make(map[key]bool, len(merged))
	for _, bead := range merged {
		k := key{Rig: bead.Rig, Bead: bead.Bead}
		if seen[k] {
			continue
		}
		seen[k] = true
		review, ok := first[k]
		if !ok {
			table.Unreviewed++
			continue
		}
		i, ok := named[review.AuthorKind]
		if !ok {
			other[review.AuthorKind]++
			continue
		}
		rows[i].Beads++
		rows[i].Reviews += lines[k]
		if review.Record.Verdict == land.VerdictApprove && !review.Record.Rejected {
			rows[i].FirstPass++
		}
	}
	table.Rows = rows
	for kind, n := range other {
		table.OtherKinds = append(table.OtherKinds, FirstPassKindRow{Kind: kind, Beads: n})
	}
	sort.Slice(table.OtherKinds, func(i, j int) bool { return table.OtherKinds[i].Kind < table.OtherKinds[j].Kind })
	return table
}

// mergedBeads returns the beads the rigs' landing logs record as merged at or
// after since, one entry per bead. A zero since is every landing the logs
// hold. A bead its rig landed more than once is counted once, under its latest
// landing.
//
// skipped is the landing-log lines that could not be read as a landing record,
// which the section names so a table built from the lines around them says how
// much of the log it could not read.
//
// A missing landings directory is an error like any other failed read: the
// review log is read from that directory first, so a section that cannot list
// it would report a town whose landing logs it could not find as a town that
// merged nothing.
//
// The caller checks Env.ReadDir and Env.ReadFile, as both section entry points
// do.
func mergedBeads(env Env, since time.Time) (merged []MergedBead, skipped int, err error) {
	dir := filepath.Join(constants.TownRuntimePath(env.Town), "landings")
	entries, err := env.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("landings directory %s: %w", dir, err)
	}
	latest := map[MergedBead]time.Time{}
	for _, entry := range entries {
		if entry.IsDir || !strings.HasSuffix(entry.Name, ".jsonl") || entry.Name == land.ReviewsFileName {
			continue
		}
		rig := strings.TrimSuffix(entry.Name, ".jsonl")
		file, err := land.RigLandingsFile(env.Town, rig)
		if err != nil {
			return nil, 0, fmt.Errorf("landings file %s: %w", entry.Name, err)
		}
		data, err := env.ReadFile(file.Path)
		if err != nil {
			return nil, 0, fmt.Errorf("landings file %s: %w", file.Path, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// A torn line is one crash wide, and the records around it are
			// still the log; internal/land reads the same file that way.
			var rec land.LandingRecord
			if json.Unmarshal([]byte(line), &rec) != nil || rec.BeadID == "" || rec.LandedAt.IsZero() {
				skipped++
				continue
			}
			if !since.IsZero() && rec.LandedAt.Before(since) {
				continue
			}
			bead := MergedBead{Rig: rig, Bead: rec.BeadID}
			if at, ok := latest[bead]; ok && at.After(rec.LandedAt) {
				continue
			}
			latest[bead] = rec.LandedAt
		}
	}
	merged = make([]MergedBead, 0, len(latest))
	for bead := range latest {
		merged = append(merged, bead)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Rig != merged[j].Rig {
			return merged[i].Rig < merged[j].Rig
		}
		return merged[i].Bead < merged[j].Bead
	})
	return merged, skipped, nil
}

// MangoFirstPassSection renders the digest's first-pass table, reading the
// town's review log and each rig's landing log through env and joining them to
// the author kind the review attribution derived for each merged bead's reviews
// (AttributeRecords), the same derived author_kind the snapshot's review group
// counts. The section reads no branch and no bead's notes of its own.
//
// An unreadable log, a store whose read failed or was not a bead list, or a
// rig that is not one path element under the town is an error: a table built
// from a read that broke would report the beads it lost as unreviewed rather
// than say the read broke. A bead the store no longer lists is an unknown
// author's, which is an answer.
func MangoFirstPassSection(ctx context.Context, env Env, since time.Time) (string, error) {
	switch {
	case env.Town == "":
		return "", errors.New("first-pass section: Env.Town is unset")
	case env.ReadFile == nil:
		return "", errors.New("first-pass section: Env.ReadFile is unset")
	case env.ReadDir == nil:
		return "", errors.New("first-pass section: Env.ReadDir is unset")
	case env.Run == nil:
		return "", errors.New("first-pass section: Env.Run is unset")
	}
	records, skippedReviews, err := townReviewRecords(env)
	if err != nil {
		return "", err
	}
	return mangoFirstPassSection(ctx, env, since, records, skippedReviews)
}

// mangoFirstPassSection renders the table from review records the caller
// already read, so one digest run reads the review log once.
func mangoFirstPassSection(ctx context.Context, env Env, since time.Time, records []land.ReviewRecord, skippedReviews int) (string, error) {
	merged, skippedLandings, err := mergedBeads(env, since)
	if err != nil {
		return "", err
	}
	reviews, err := firstPassReviews(ctx, env, records, merged)
	if err != nil {
		return "", err
	}
	return firstPassSectionText(CountFirstPass(merged, reviews), since, skippedReviews, skippedLandings), nil
}

// firstPassReviews joins the review records of the merged beads to the derived
// author_kind the review attribution gives each of them (AttributeRecords).
// Only the merged beads' records are derived: a review of a bead that never
// merged counts in no row, so it costs no store read.
func firstPassReviews(ctx context.Context, env Env, records []land.ReviewRecord, merged []MergedBead) ([]AttributedRecord, error) {
	landed := make(map[MergedBead]bool, len(merged))
	for _, bead := range merged {
		landed[bead] = true
	}
	wanted := make([]land.ReviewRecord, 0, len(records))
	for _, rec := range records {
		if landed[MergedBead{Rig: rec.Rig, Bead: rec.BeadID}] {
			wanted = append(wanted, rec)
		}
	}
	return AttributeRecords(ctx, env, wanted)
}

// firstPassSectionText renders the table, the window it covers and the merged
// beads it could not put in a row.
func firstPassSectionText(table FirstPassTable, since time.Time, skippedReviews, skippedLandings int) string {
	window := firstPassWindow(since)
	var b strings.Builder
	b.WriteString("### First-pass rate by author kind\n\n")
	b.WriteString("Window: " + window + ". A bead is a first pass when the first om review it got is " +
		"an approve the landing did not turn away. Author kind is the one the review attribution " +
		"derived for that review from the bead's branch, so the table counts the kinds the review " +
		"data already carries. Rate is first pass over merged beads; reviews per bead is the review " +
		"lines those beads carry over the same count.\n")
	if table.total() > 0 {
		b.WriteString("\n" + firstPassTableText(table))
	} else {
		b.WriteString("\n\nNo merged bead landed in the window: no first-pass rate to read.")
	}
	if table.Unreviewed > 0 {
		b.WriteString(fmt.Sprintf("\n\n%d merged bead(s) carry no review line and count in no row.", table.Unreviewed))
	}
	for _, row := range table.OtherKinds {
		b.WriteString(fmt.Sprintf("\n\n%d merged bead(s) are the %s's, a kind this table does not name.", row.Beads, row.Kind))
	}
	if skippedReviews > 0 {
		b.WriteString(fmt.Sprintf("\n\n%d review-log line(s) were not readable as records and count in no bead's reviews.", skippedReviews))
	}
	if skippedLandings > 0 {
		b.WriteString(fmt.Sprintf("\n\n%d landing-record line(s) were not readable and count as no bead's merge.", skippedLandings))
	}
	return b.String()
}

// total is how many merged beads the table accounts for, in a row or named
// beneath it.
func (t FirstPassTable) total() int {
	n := t.Unreviewed
	for _, row := range t.Rows {
		n += row.Beads
	}
	for _, row := range t.OtherKinds {
		n += row.Beads
	}
	return n
}

// firstPassWindow names the merged beads the table covers.
func firstPassWindow(since time.Time) string {
	if since.IsZero() {
		return "all merged beads"
	}
	return "merged beads landed at or after " + since.UTC().Format(time.RFC3339)
}

// firstPassTableText renders the rows as the report's Markdown table. A row
// with no merged bead reads "n/a" rather than 0%, because no bead is not a
// rate of zero.
func firstPassTableText(table FirstPassTable) string {
	lines := []string{
		"| author kind | merged beads | first pass | first-pass rate | reviews | reviews per bead |",
		"|---|---|---|---|---|---|",
	}
	for _, row := range table.Rows {
		lines = append(lines, "| "+strings.Join([]string{
			row.Kind,
			strconv.Itoa(row.Beads),
			strconv.Itoa(row.FirstPass),
			firstPassRate(row),
			strconv.Itoa(row.Reviews),
			firstPassPerBead(row),
		}, " | ")+" |")
	}
	return strings.Join(lines, "\n")
}

// firstPassRate is the row's first passes over its merged beads.
func firstPassRate(row FirstPassKindRow) string {
	if row.Beads == 0 {
		return "n/a"
	}
	return rejectionPercent(fraction(row.FirstPass, row.Beads))
}

// firstPassPerBead is the row's review lines over its merged beads.
func firstPassPerBead(row FirstPassKindRow) string {
	if row.Beads == 0 {
		return "n/a"
	}
	return strconv.FormatFloat(float64(row.Reviews)/float64(row.Beads), 'f', 1, 64)
}

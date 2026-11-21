package cmd

// The recurrence report behind `gt land om-stats --recurrence`: per hold-out
// class, the held-out and filed minor findings and how many of them a later
// review restated (gt-3ohcu0), and the JSON that prints them (gt-5hg00y; the
// table is the next part).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/omlessons"
	"github.com/steveyegge/gastown/internal/workspace"
)

// landOmStatsRecurrenceFollowDays is how long after a finding's own record a
// later record may restate it and still count as the follow: the hold-out
// withholds the follow-up for four weeks (Q37).
const landOmStatsRecurrenceFollowDays = 28

// landOmStatsRecurrenceSide is one side's tally and the figures derived from it.
type landOmStatsRecurrenceSide struct {
	Findings int     `json:"findings"`
	Followed int     `json:"followed"`
	Rate     float64 `json:"rate"`
}

// finish derives a side's rate inside recurrence, so the table and the JSON read
// one filled row and cannot disagree.
func (s *landOmStatsRecurrenceSide) finish() {
	if s.Findings > 0 {
		s.Rate = float64(s.Followed) / float64(s.Findings)
	}
}

// landOmStatsRecurrenceClass is one hold-out class's two sides.
type landOmStatsRecurrenceClass struct {
	ID      string                    `json:"id"`
	HeldOut landOmStatsRecurrenceSide `json:"held_out"`
	Filed   landOmStatsRecurrenceSide `json:"filed"`
}

// recurrence tallies each hold-out class's minor findings whose record time
// falls inside the half-open hold-out window: held out or filed by where
// omlessons.HeldOut puts the review's bead (Q37), counted when minor and
// Classify names the class, the only severity the follow-up feed files a bead
// for (gt-oi4je). A record is evidence when it names a bead, is not a queue
// line (gt-3arxj), and its time is not after now: a record the report cannot
// yet have seen is no evidence, and now is the only clock (gt-c9sr9p). A
// counted finding is followed when a later evidence record of another bead
// restates it (gt-o5uvti).
func recurrence(records []land.ReviewRecord, now time.Time) []landOmStatsRecurrenceClass {
	start, end := omlessons.HoldOutStart, omlessons.HoldOutStart.AddDate(0, 0, omlessons.HoldOutDays)
	index := landOmStatsRecurrenceIndex(records, now)
	rows := make([]landOmStatsRecurrenceClass, 0, len(omlessons.HoldOutClasses))
	for _, class := range omlessons.HoldOutClasses {
		row := landOmStatsRecurrenceClass{ID: class}
		for _, rec := range records {
			if rec.Queue != nil || rec.BeadID == "" || rec.At.After(now) || rec.At.Before(start) || !rec.At.Before(end) {
				continue
			}
			side := &row.Filed
			if omlessons.HeldOut(rec.BeadID, class, rec.At) {
				side = &row.HeldOut
			}
			for _, f := range rec.FindingText {
				if strings.ToLower(strings.TrimSpace(f.Severity)) != "minor" || omlessons.Classify(f) != class {
					continue
				}
				side.Findings++
				if landOmStatsRecurrenceFollowed(f, rec, index) {
					side.Followed++
				}
			}
		}
		row.HeldOut.finish()
		row.Filed.finish()
		rows = append(rows, row)
	}
	return rows
}

// landOmStatsRecurrenceEvidence is one finding an evidence record named, with
// the record's bead and time: one entry of the follow scan's index.
type landOmStatsRecurrenceEvidence struct {
	bead    string
	at      time.Time
	finding land.ReviewFindingText
}

// landOmStatsRecurrenceIndex groups evidence records' findings by file once per
// recurrence call, so each follow lookup reads only the entries naming the
// finding's file instead of every record (om b0e03f0648e2). It applies the
// evidence test whole: a record names a bead, is not a queue line, and its time
// is not after now (gt-c9sr9p).
func landOmStatsRecurrenceIndex(records []land.ReviewRecord, now time.Time) map[string][]landOmStatsRecurrenceEvidence {
	index := make(map[string][]landOmStatsRecurrenceEvidence)
	for _, rec := range records {
		if rec.Queue != nil || rec.BeadID == "" || rec.At.After(now) {
			continue
		}
		for _, f := range rec.FindingText {
			index[f.File] = append(index[f.File], landOmStatsRecurrenceEvidence{bead: rec.BeadID, at: rec.At, finding: f})
		}
	}
	return index
}

// landOmStatsRecurrenceFollowed reports whether f, a finding of rec, is
// followed: an evidence record of a different bead, strictly later than rec and
// at most landOmStatsRecurrenceFollowDays after it, names a finding on the same
// file that metrics.SimilarFindings matches, whatever its class or severity.
// The index holds evidence records only, so a later record read from it is not
// after now and names a bead.
func landOmStatsRecurrenceFollowed(f land.ReviewFindingText, rec land.ReviewRecord, index map[string][]landOmStatsRecurrenceEvidence) bool {
	last := rec.At.AddDate(0, 0, landOmStatsRecurrenceFollowDays)
	for _, e := range index[f.File] {
		if e.bead != rec.BeadID && e.at.After(rec.At) && !e.at.After(last) && metrics.SimilarFindings(f, e.finding) {
			return true
		}
	}
	return false
}

// landOmStatsRecurrenceDoc is the --recurrence report as --json prints it.
type landOmStatsRecurrenceDoc struct {
	land.OmStats
	Recurrence []landOmStatsRecurrenceClass `json:"recurrence"`
}

// landOmStatsRecurrenceReport is the command body with --recurrence set.
func landOmStatsRecurrenceReport(out io.Writer, sinceValue, recordsFlag, logFlag string, asJSON bool) error {
	return landOmStatsRecurrenceReportFrom(landOmStatsDisk{}, workspace.FindFromCwdOrError, out, sinceValue, recordsFlag, logFlag, asJSON, time.Now())
}

// landOmStatsRecurrenceReportFrom prints the base report and then the
// recurrence section, both from one read of the records (gt-4be1z5), with now
// the only clock (gt-c9sr9p).
func landOmStatsRecurrenceReportFrom(fsys landOmStatsFS, findTown func() (string, error), out io.Writer, sinceValue, recordsFlag, logFlag string, asJSON bool, now time.Time) error {
	since, err := parseLandOmStatsSince(sinceValue)
	if err != nil {
		return err
	}
	inputs, err := landOmStatsResolvePaths(recordsFlag, logFlag, findTown)
	if err != nil {
		return err
	}
	data, err := readLandOmStatsRecords(fsys, inputs)
	if err != nil {
		return err
	}
	logFile, closeLog, err := openLandOmStatsSource(fsys, inputs.log, inputs.logNamed)
	if err != nil {
		return err
	}
	defer closeLog()
	stats, err := land.AggregateOmStats(bytes.NewReader(data), logFile, since)
	if err != nil {
		return err
	}
	// The second value counts the lines the reader could not use: the base
	// report above counts the same lines as its Skipped, so a torn log is said
	// there and is not a failure here; recurrence reads the records that parsed.
	records, unusable := readLandOmStatsClassRecords(data)
	if len(records) == 0 && unusable > 0 {
		return fmt.Errorf("the review records hold %d lines that do not parse and none that do, so no recurrence can be counted", unusable)
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(landOmStatsRecurrenceDoc{OmStats: stats, Recurrence: recurrence(records, now)}); err != nil {
			return fmt.Errorf("writing the report: %w", err)
		}
		return nil
	}
	writeLandOmStatsTable(out, stats, since)
	return writeLandOmStatsRecurrenceTable(out, recurrence(records, now))
}

// writeLandOmStatsRecurrenceTable prints each hold-out class's two sides.
func writeLandOmStatsRecurrenceTable(w io.Writer, rows []landOmStatsRecurrenceClass) error {
	fmt.Fprintln(w, "\nRecurrence of minor findings within 28 days of their own review")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Class\tHeld out\tHeld followed\tHeld rate\tFiled\tFiled followed\tFiled rate")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%d\t%d\t%s\n", row.ID,
			row.HeldOut.Findings, row.HeldOut.Followed, landOmStatsPercent(row.HeldOut.Rate),
			row.Filed.Findings, row.Filed.Followed, landOmStatsPercent(row.Filed.Rate))
	}
	return tw.Flush()
}

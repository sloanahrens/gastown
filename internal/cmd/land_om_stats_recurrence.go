package cmd

// The recurrence comparison behind `gt land om-stats --recurrence` (gt-3ohcu0); the flag and the printing are later beads.

import (
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/omlessons"
)

// landOmStatsRecurrenceFollowDays is how long after a finding's own record a
// later record may restate it and still count as the follow: the hold-out
// withholds the follow-up for four weeks (Q37).
const landOmStatsRecurrenceFollowDays = 28

// landOmStatsRecurrenceSide is one side's tally: findings and how many a later
// review restated.
type landOmStatsRecurrenceSide struct {
	Findings int `json:"findings"`
	Followed int `json:"followed"`
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

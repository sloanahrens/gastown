package cmd

// The recurrence comparison behind `gt land om-stats --recurrence` (gt-3ohcu0); the flag and the printing are later beads.

import (
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

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
// for (gt-oi4je). A record is evidence when it names a bead and is not a queue
// line (gt-3arxj). The follow scan and the after-now rule are later beads, so
// Followed stays 0.
func recurrence(records []land.ReviewRecord, _ time.Time) []landOmStatsRecurrenceClass {
	start, end := omlessons.HoldOutStart, omlessons.HoldOutStart.AddDate(0, 0, omlessons.HoldOutDays)
	rows := make([]landOmStatsRecurrenceClass, 0, len(omlessons.HoldOutClasses))
	for _, class := range omlessons.HoldOutClasses {
		row := landOmStatsRecurrenceClass{ID: class}
		for _, rec := range records {
			if rec.Queue != nil || rec.BeadID == "" || rec.At.Before(start) || !rec.At.Before(end) {
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
			}
		}
		rows = append(rows, row)
	}
	return rows
}

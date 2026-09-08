package doltserver

import (
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// Hold labels protect Dolt databases from destructive cleanup (gt-61x).
//
// A bead carrying the label "dolt-hold:<dbname>" holds that specific database:
// gt dolt cleanup will refuse to remove it, even with --force, until the bead
// is closed. A bead carrying the bare "dolt-hold" label is a blanket hold that
// protects every database. Holds are read from the town-level (hq) beads
// database, where cross-rig operational decisions live.
const (
	// HoldLabel is the blanket hold label: protects all databases.
	HoldLabel = "dolt-hold"
	// HoldLabelPrefix prefixes per-database hold labels ("dolt-hold:<dbname>").
	HoldLabelPrefix = HoldLabel + ":"
)

// DatabaseHold describes an active hold protecting one or all databases.
type DatabaseHold struct {
	BeadID string // bead carrying the hold label
	Title  string // bead title, for operator-facing messages
	All    bool   // blanket hold: protects every database
	DB     string // specific database name (when !All)
}

// FindDatabaseHolds queries town-level beads for active database holds.
// Any bead that is not closed counts — a hold stays in force while the
// decision it represents is pending, whatever its workflow status.
func FindDatabaseHolds(townRoot string) ([]DatabaseHold, error) {
	b := beads.New(townRoot)
	issues, err := b.ListIssueStatuses(
		beads.StatusOpen,
		beads.StatusInProgress,
		beads.StatusBlocked,
		beads.StatusDeferred,
		beads.IssueStatusPinned,
		beads.IssueStatusHooked,
	)
	if err != nil {
		return nil, err
	}
	return holdsFromIssues(issues), nil
}

// holdsFromIssues extracts database holds from issue labels.
func holdsFromIssues(issues []*beads.Issue) []DatabaseHold {
	var holds []DatabaseHold
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		for _, label := range issue.Labels {
			switch {
			case label == HoldLabel:
				holds = append(holds, DatabaseHold{BeadID: issue.ID, Title: issue.Title, All: true})
			case strings.HasPrefix(label, HoldLabelPrefix):
				db := strings.TrimPrefix(label, HoldLabelPrefix)
				if db != "" {
					holds = append(holds, DatabaseHold{BeadID: issue.ID, Title: issue.Title, DB: db})
				}
			}
		}
	}
	return holds
}

// HoldFor returns the hold protecting dbName, or nil if none applies.
// A blanket hold (All) protects every database.
func HoldFor(holds []DatabaseHold, dbName string) *DatabaseHold {
	for i := range holds {
		if holds[i].All || holds[i].DB == dbName {
			return &holds[i]
		}
	}
	return nil
}

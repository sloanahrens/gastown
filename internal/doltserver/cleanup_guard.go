package doltserver

import (
	"fmt"
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

// ForceAuthLabel marks a bead as a genuine authorization record for a forced
// Dolt cleanup by an agent (gt-2oy). checkAgentForceAuthorization used to
// accept any existing bead ID, so an agent could point --authorized-by at any
// bead — including one it had just created itself.
const ForceAuthLabel = "dolt-force-auth"

// ValidateForceAuthorization checks that the --authorized-by bead is a genuine
// authorization record (gt-2oy): it must carry the ForceAuthLabel label, must
// not be closed, and must not have been created by the requesting actor itself.
func ValidateForceAuthorization(issue *beads.Issue, actor string) error {
	if issue == nil {
		return fmt.Errorf("authorization bead not found")
	}
	labeled := false
	for _, label := range issue.Labels {
		if label == ForceAuthLabel {
			labeled = true
			break
		}
	}
	if !labeled {
		return fmt.Errorf(`bead %s is not an authorization record: missing the %q label (gt-2oy)

An authorization bead must be created by the mayor/overseer with:
  bd update %s --labels=%s   # or create a new bead carrying that label`,
			issue.ID, ForceAuthLabel, issue.ID, ForceAuthLabel)
	}
	if beads.IssueStatus(issue.Status).IsTerminal() {
		return fmt.Errorf("authorization bead %s is %s — a forced cleanup requires an open authorization (gt-2oy)",
			issue.ID, issue.Status)
	}
	if actor != "" && issue.CreatedBy == actor {
		return fmt.Errorf("authorization bead %s was created by %s itself — self-authorization is not allowed (gt-2oy); the bead must record a mayor/overseer decision",
			issue.ID, actor)
	}
	return nil
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

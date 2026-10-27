package api

import "time"

// SnapshotRequest asks for one page of the town snapshot. Limit 0 takes the
// server's default page size; Cursor empty reads the first page, and any other
// value is the previous page's NextCursor.
type SnapshotRequest struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// TownSnapshot is one page of the town's issues with the counts over the whole
// set, all read in one transaction, so the page, the blocked verdicts and the
// counts are one moment and never disagree.
//
// It is one page, not the whole town: Truncated says this page was cut,
// NextCursor is where the next page starts, and Counts still describes every
// issue, so a cut page keeps exact totals. Each page is its own moment — a
// walk that follows the cursor reads a later At, and may see an issue written
// between two pages — so a walk over several pages is not one consistent view
// of the town. A caller that needs the whole town in one moment asks for it in
// one page.
type TownSnapshot struct {
	// At is the moment this page was read, taken from the database's clock.
	At time.Time `json:"at"`
	// Issues is this page of durable issues, in list order (priority, then
	// creation time, then id), each with the blocking verdict.
	Issues []SnapshotIssue `json:"issues"`
	// Counts describes every durable issue, not only this page's.
	Counts SnapshotCounts `json:"counts"`
	// Truncated is true when more issues remain past this page.
	Truncated bool `json:"truncated"`
	// NextCursor resumes after this page; empty when Truncated is false.
	NextCursor string `json:"next_cursor,omitempty"`
}

// SnapshotIssue is one issue of a snapshot: the list row plus the blocking
// verdict. Blocked is true when the issue, or a parent above it, has an open
// blocker; BlockedBy names the issue's own direct open blockers.
type SnapshotIssue struct {
	Issue
	Blocked bool `json:"blocked"`
}

// SnapshotCounts counts the town's durable issues (wisps are not counted):
// Total, the totals by status and by rig, and how many are blocked, by rig.
// The maps are never nil.
type SnapshotCounts struct {
	Total       int            `json:"total"`
	ByStatus    map[string]int `json:"by_status"`
	ByRig       map[string]int `json:"by_rig"`
	Blocked     int            `json:"blocked"`
	BlockedRigs map[string]int `json:"blocked_rigs"`
}

// Package migrate moves a rig's data from its Dolt database (beads) into the
// tracker: Read turns the Dolt tables into a Dataset, Import writes a Dataset
// into Postgres in one transaction, and Verify compares the two. Mapping rules
// that need no database live in mapping.go and are unit-tested.
package migrate

import "time"

// IssueRow is one row of Dolt's issues or wisps table, with the columns the
// tracker keeps. Columns it has no home for travel in Legacy.
type IssueRow struct {
	ID, Title, Description, Design, Notes, Acceptance string
	Status, Type, Assignee, Owner, CreatedBy          string
	CloseReason, WispType, EventKind, Payload         string
	HookBead, AgentState                              string
	Priority                                          int
	CreatedAt, UpdatedAt                              time.Time
	StartedAt, ClosedAt, DeferUntil, LastActivity     *time.Time
	// Wisp marks a row read from the wisps table: ephemeral in the tracker.
	Wisp bool
	// Legacy holds the populated columns the tracker schema has no column
	// for (row_lock, content_hash, ...), kept in issues.metadata.
	Legacy map[string]any
}

// DepRow is one dependency link from Dolt's dependencies or wisp_dependencies.
// DependsOn is whichever target column was set; a bare id of another rig
// (depends_on_external) is kept as it is.
type DepRow struct {
	ID, IssueID, DependsOn, Type, ThreadID, CreatedBy string
	Metadata                                          string
	CreatedAt                                         time.Time
}

// LabelRow is one label on an issue or wisp.
type LabelRow struct{ IssueID, Label string }

// CommentRow is one comment on an issue or wisp.
type CommentRow struct {
	ID, IssueID, Author, Text string
	CreatedAt                 time.Time
}

// EventRow is one audit event from Dolt's events or wisp_events.
type EventRow struct {
	ID, IssueID, Op, Actor string
	OldValue, NewValue     *string
	Comment                *string
	CreatedAt              time.Time
}

// CustomStatus is a row of custom_statuses.
type CustomStatus struct{ Name, Category string }

// Dataset is everything read from one rig's Dolt database.
type Dataset struct {
	Issues         []IssueRow
	Deps           []DepRow
	Labels         []LabelRow
	Comments       []CommentRow
	Events         []EventRow
	Config         map[string]string
	CustomTypes    []string
	CustomStatuses []CustomStatus
	ChildCounters  map[string]int
}

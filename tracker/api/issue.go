package api

import (
	"encoding/json"
	"time"
)

// Issue is an issue or wisp on the wire. The JSON names are beads.Issue's, so
// the gt-side adapter is a field copy; times are RFC3339 here where beads.Issue
// carries strings. Fields no caller reads (external_ref, blocks, event_kind
// on the bead itself, blocked_by_count) are not carried.
type Issue struct {
	ID                 string     `json:"id"`
	Title              string     `json:"title"`
	Description        string     `json:"description"`
	Design             string     `json:"design,omitempty"`
	Notes              string     `json:"notes,omitempty"`
	AcceptanceCriteria string     `json:"acceptance_criteria,omitempty"`
	Status             string     `json:"status"`
	Priority           int        `json:"priority"`
	Type               string     `json:"issue_type"`
	Assignee           string     `json:"assignee,omitempty"`
	Owner              string     `json:"owner,omitempty"`
	CreatedBy          string     `json:"created_by,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	ClosedAt           *time.Time `json:"closed_at,omitempty"`
	DeferUntil         *time.Time `json:"defer_until,omitempty"`
	CloseReason        string     `json:"close_reason,omitempty"`
	Parent             string     `json:"parent,omitempty"`
	Children           []string   `json:"children,omitempty"`
	DependsOn          []string   `json:"depends_on,omitempty"`
	BlockedBy          []string   `json:"blocked_by,omitempty"`
	Labels             []string   `json:"labels,omitempty"`
	Ephemeral          bool       `json:"ephemeral,omitempty"`
	WispType           string     `json:"wisp_type,omitempty"`

	// Event beads (issue_type "event").
	EventKind string `json:"event_kind,omitempty"`
	Payload   string `json:"payload,omitempty"`

	// Agent beads (issue_type "agent").
	HookBead     string     `json:"hook_bead,omitempty"`
	AgentState   string     `json:"agent_state,omitempty"`
	LastActivity *time.Time `json:"last_activity,omitempty"`

	// Dependencies are what the issue depends on, each with its relation,
	// title and status. Targets that are not issues of this database
	// (external references) are left out. Only a read of named issues
	// (GetIssues) fills them, and BlockedBy there: the open targets of its
	// `blocks` links. A list leaves both empty, and Blocked fills BlockedBy.
	Dependencies []IssueDep `json:"dependencies,omitempty"`
	// Dependents are the issues that depend on this one, with their relation:
	// the other end of each link. Filled with Dependencies, by a read of named
	// issues only.
	Dependents []IssueDep `json:"dependents,omitempty"`

	// Counts the list endpoints fill in without loading the rows.
	CommentCount    int `json:"comment_count,omitempty"`
	DependencyCount int `json:"dependency_count,omitempty"`
	DependentCount  int `json:"dependent_count,omitempty"`
}

// Comment is one comment on an issue.
type Comment struct {
	ID        string    `json:"id"`
	IssueID   string    `json:"issue_id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// IssueDep is an issue another depends on, with the relation between them.
type IssueDep struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	Priority       int    `json:"priority"`
	Type           string `json:"issue_type"`
	DependencyType string `json:"dependency_type"`
	CloseReason    string `json:"close_reason,omitempty"`
}

// Event is one row of the append-only events feed.
type Event struct {
	Seq      int64           `json:"seq"`
	TS       time.Time       `json:"ts"`
	Op       string          `json:"op"`
	IssueID  string          `json:"issue_id"`
	Actor    string          `json:"actor,omitempty"`
	OldValue *string         `json:"old_value,omitempty"`
	NewValue *string         `json:"new_value,omitempty"`
	Comment  *string         `json:"comment,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// IssuePage is one page of a list. NextCursor is empty on the last page; a
// list never stops silently at a cap.
//
// RigErrors is set on a list that named several rigs (ListFilter.Rigs): each
// rig the tracker does not hold is in it with why, and the issues answer every
// other rig. It is the per-rig channel the board's Errors is for, so one
// unknown rig does not refuse the rest.
type IssuePage struct {
	Issues     []Issue           `json:"issues"`
	NextCursor string            `json:"next_cursor,omitempty"`
	RigErrors  map[string]string `json:"rig_errors,omitempty"`
}

// EventsPage is one page of the events feed after a seq. NextAfter is the seq
// to ask for next. FirstSeq is the oldest seq the server still has, so a
// client that asked from before it knows the feed was pruned past it.
type EventsPage struct {
	Events    []Event `json:"events"`
	NextAfter int64   `json:"next_after"`
	FirstSeq  int64   `json:"first_seq"`
}

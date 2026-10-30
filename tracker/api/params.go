package api

import "time"

// CreateParams creates an issue. Rig is required unless ID is given; an ID
// must start with its rig's prefix and a dash. Actor becomes created_by.
type CreateParams struct {
	ID           string   `json:"id,omitempty"`
	Rig          string   `json:"rig,omitempty"`
	Title        string   `json:"title"`
	Type         string   `json:"issue_type,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	Priority     int      `json:"priority"`
	Description  string   `json:"description,omitempty"`
	Acceptance   string   `json:"acceptance_criteria,omitempty"`
	Parent       string   `json:"parent,omitempty"`
	Assignee     string   `json:"assignee,omitempty"`
	Actor        string   `json:"actor,omitempty"`
	Ephemeral    bool     `json:"ephemeral,omitempty"`
	EventKind    string   `json:"event_kind,omitempty"`
	EventPayload string   `json:"event_payload,omitempty"`
}

// ListFilter selects issues. The zero value matches every non-ephemeral
// issue that is not an infrastructure type. Statuses are ORed; Labels are
// ANDed. Limit 0 means no limit (a page is still bounded by the server, and
// NextCursor says so).
type ListFilter struct {
	Statuses     []string `json:"statuses,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	Priority     *int     `json:"priority,omitempty"`
	Parent       string   `json:"parent,omitempty"`
	Assignee     string   `json:"assignee,omitempty"`
	NoAssignee   bool     `json:"no_assignee,omitempty"`
	IssueType    string   `json:"issue_type,omitempty"`
	Rig          string   `json:"rig,omitempty"`
	IDs          []string `json:"ids,omitempty"`
	Ephemeral    bool     `json:"ephemeral,omitempty"`
	IncludeInfra bool     `json:"include_infra,omitempty"`
	// ClosedAfter and ClosedBefore keep the issues closed at or after, and
	// before, those instants; an issue that is not closed has no close time,
	// so neither bound matches one.
	ClosedAfter  time.Time `json:"closed_after,omitzero"`
	ClosedBefore time.Time `json:"closed_before,omitzero"`
	// CreatedAfter and CreatedBefore bound creation the same way.
	CreatedAfter  time.Time `json:"created_after,omitzero"`
	CreatedBefore time.Time `json:"created_before,omitzero"`
	// Limit and Cursor page a list. A count carries neither: see ValidateCount.
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// UpdatePatch changes the fields it sets. A nil pointer leaves a field alone.
// AppendNotes adds a line to the notes; Persistent promotes a wisp.
type UpdatePatch struct {
	Title        *string  `json:"title,omitempty"`
	Status       *string  `json:"status,omitempty"`
	Priority     *int     `json:"priority,omitempty"`
	Description  *string  `json:"description,omitempty"`
	Assignee     *string  `json:"assignee,omitempty"`
	Acceptance   *string  `json:"acceptance_criteria,omitempty"`
	AddLabels    []string `json:"add_labels,omitempty"`
	RemoveLabels []string `json:"remove_labels,omitempty"`
	SetLabels    []string `json:"set_labels,omitempty"`
	AppendNotes  string   `json:"append_notes,omitempty"`
	Persistent   bool     `json:"persistent,omitempty"`
	// DeferUntil sets when a deferred or open issue becomes workable again; a
	// pointer to the zero time clears it.
	DeferUntil *time.Time `json:"defer_until,omitempty"`
	Force      bool       `json:"force,omitempty"`
	Actor      string     `json:"actor,omitempty"`
}

// Guard makes a write conditional: it applies only while the issue's assignee
// (and, if set, status) still equal the expected values. A guard that does not
// hold returns a guard_failed error and writes nothing. The zero Guard is no
// guard.
type Guard struct {
	IfAssignee *string `json:"if_assignee,omitempty"`
	IfStatus   *string `json:"if_status,omitempty"`
}

// ReadyFilter selects ready or blocked work. Parent and Molecule scope to a
// subtree; Unassigned and the Exclude lists narrow the board; Gated includes
// work behind a gate.
type ReadyFilter struct {
	Rig           string   `json:"rig,omitempty"`
	Parent        string   `json:"parent,omitempty"`
	Molecule      string   `json:"molecule,omitempty"`
	Unassigned    bool     `json:"unassigned,omitempty"`
	ExcludeLabels []string `json:"exclude_labels,omitempty"`
	ExcludeTypes  []string `json:"exclude_types,omitempty"`
	Gated         bool     `json:"gated,omitempty"`
	Limit         int      `json:"limit,omitempty"`
	Cursor        string   `json:"cursor,omitempty"`
}

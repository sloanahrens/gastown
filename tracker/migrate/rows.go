package migrate

import (
	"fmt"
	"math"
	"time"
)

// The Dolt driver returns text as []byte, integers as int64 and datetimes as
// time.Time (with parseTime), and NULL as nil. These helpers read a generic
// row (column name -> value) without caring which.

func str(row map[string]any, col string) string {
	switch v := row[col].(type) {
	case nil:
		return ""
	case []byte:
		return string(v)
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func strPtr(row map[string]any, col string) *string {
	if row[col] == nil {
		return nil
	}
	s := str(row, col)
	return &s
}

func integer(row map[string]any, col string) int {
	switch v := row[col].(type) {
	case int64:
		return clampInt(v)
	case int:
		return v
	case int32:
		return clampInt(int64(v))
	case uint64:
		if v > math.MaxInt32 {
			return math.MaxInt32
		}
		return int(v) // below MaxInt32, so it fits
	case float64:
		return clampInt(int64(v))
	}
	return 0
}

// clampInt narrows v to an int within the 32-bit range: the columns read this
// way are priorities and counters, never anywhere near it.
func clampInt(v int64) int {
	switch {
	case v > math.MaxInt32:
		return math.MaxInt32
	case v < math.MinInt32:
		return math.MinInt32
	}
	return int(v)
}

// datetime reads a datetime column. The driver returns a time.Time when the
// connection parses times and the text of the value ("2006-01-02 15:04:05",
// with optional fractional seconds) when it does not; both are read, as UTC.
// A NULL, an empty value or text that is not a datetime is the zero time.
func datetime(v any) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x
	case []byte:
		return datetime(string(x))
	case string:
		for _, layout := range []string{"2006-01-02 15:04:05.999999999", time.RFC3339Nano} {
			if t, err := time.ParseInLocation(layout, x, time.UTC); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

func timeVal(row map[string]any, col string) time.Time { return datetime(row[col]) }

func timePtr(row map[string]any, col string) *time.Time {
	t := datetime(row[col])
	if t.IsZero() {
		return nil
	}
	return &t
}

// issueColumns are the columns of issues/wisps with their own field in
// IssueRow, or that are derived or implied and so are dropped (is_blocked is
// recomputed; ephemeral follows the table). Every other populated column goes
// to Legacy.
var issueColumns = map[string]bool{
	"id": true, "title": true, "description": true, "design": true, "notes": true, "acceptance_criteria": true,
	"status": true, "priority": true, "issue_type": true, "assignee": true, "owner": true, "created_by": true,
	"created_at": true, "updated_at": true, "started_at": true, "closed_at": true, "defer_until": true,
	"close_reason": true, "wisp_type": true, "event_kind": true, "payload": true, "hook_bead": true,
	"agent_state": true, "last_activity": true,
	"is_blocked": true, "ephemeral": true,
}

// issueFromRow maps a row of Dolt's issues (wisp=false) or wisps (wisp=true)
// table.
func issueFromRow(row map[string]any, wisp bool) IssueRow {
	is := IssueRow{
		ID: str(row, "id"), Title: str(row, "title"), Description: str(row, "description"), Design: str(row, "design"),
		Notes: str(row, "notes"), Acceptance: str(row, "acceptance_criteria"), Status: str(row, "status"),
		Type: str(row, "issue_type"), Assignee: str(row, "assignee"), Owner: str(row, "owner"), CreatedBy: str(row, "created_by"),
		CloseReason: str(row, "close_reason"), WispType: str(row, "wisp_type"), EventKind: str(row, "event_kind"),
		Payload: str(row, "payload"), HookBead: str(row, "hook_bead"), AgentState: str(row, "agent_state"),
		Priority: integer(row, "priority"), CreatedAt: timeVal(row, "created_at"), UpdatedAt: timeVal(row, "updated_at"),
		StartedAt: timePtr(row, "started_at"), ClosedAt: timePtr(row, "closed_at"), DeferUntil: timePtr(row, "defer_until"),
		LastActivity: timePtr(row, "last_activity"), Wisp: wisp, Legacy: map[string]any{},
	}
	for col, v := range row {
		if issueColumns[col] {
			continue
		}
		switch x := v.(type) {
		case nil:
		case []byte:
			if len(x) > 0 {
				is.Legacy[col] = string(x)
			}
		case string:
			if x != "" {
				is.Legacy[col] = x
			}
		case time.Time:
			if !x.IsZero() {
				is.Legacy[col] = x.UTC().Format(time.RFC3339Nano)
			}
		case bool:
			if x {
				is.Legacy[col] = x
			}
		case int64:
			if x != 0 {
				is.Legacy[col] = x
			}
		case float64:
			if x != 0 {
				is.Legacy[col] = x
			}
		default:
			is.Legacy[col] = x
		}
	}
	return is
}

// depFromRow maps a row of dependencies or wisp_dependencies.
func depFromRow(row map[string]any) DepRow {
	return DepRow{
		ID: str(row, "id"), IssueID: str(row, "issue_id"), Type: str(row, "type"), ThreadID: str(row, "thread_id"),
		DependsOn: depTarget(str(row, "depends_on_issue_id"), str(row, "depends_on_wisp_id"), str(row, "depends_on_external")),
		CreatedAt: timeVal(row, "created_at"), CreatedBy: str(row, "created_by"), Metadata: str(row, "metadata"),
	}
}

// eventFromRow maps a row of events or wisp_events.
func eventFromRow(row map[string]any) EventRow {
	return EventRow{
		ID: str(row, "id"), IssueID: str(row, "issue_id"), Op: str(row, "event_type"), Actor: str(row, "actor"),
		OldValue: strPtr(row, "old_value"), NewValue: strPtr(row, "new_value"), Comment: strPtr(row, "comment"),
		CreatedAt: timeVal(row, "created_at"),
	}
}

// commentFromRow maps a row of comments or wisp_comments.
func commentFromRow(row map[string]any) CommentRow {
	return CommentRow{ID: str(row, "id"), IssueID: str(row, "issue_id"), Author: str(row, "author"), Text: str(row, "text"), CreatedAt: timeVal(row, "created_at")}
}

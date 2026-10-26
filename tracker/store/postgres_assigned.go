package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/tracker/api"
)

// Assigned returns one page of the issues whose assignee is in req.Assignees
// and whose status is in req.Statuses, keyed by assignee, in list order.
func (p *Postgres) Assigned(ctx context.Context, req api.AssignedRequest) (api.AssignedPage, error) {
	if err := req.Validate(); err != nil {
		return api.AssignedPage{}, err
	}
	statuses := uniqueStrings(req.Statuses)
	if err := p.validateAssignedStatuses(ctx, statuses); err != nil {
		return api.AssignedPage{}, err
	}
	cur, err := decodeListCursor(req.Cursor)
	if err != nil {
		return api.AssignedPage{}, err
	}
	size := pageSize(req.Limit)
	q, args := buildAssignedQuery(uniqueStrings(req.Assignees), statuses, req.Rig, req.IncludeWisps, cur, size)
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return api.AssignedPage{}, fmt.Errorf("store: assigned: %w", err)
	}
	defer rows.Close()
	var issues []api.Issue
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return api.AssignedPage{}, fmt.Errorf("store: assigned: %w", err)
		}
		issues = append(issues, is)
	}
	if err := rows.Err(); err != nil {
		return api.AssignedPage{}, fmt.Errorf("store: assigned: %w", err)
	}
	page := api.AssignedPage{Assigned: map[string][]api.Issue{}}
	if len(issues) > size {
		issues = issues[:size]
		last := issues[size-1]
		page.NextCursor = listCursor{Priority: last.Priority, CreatedAt: last.CreatedAt, ID: last.ID}.encode()
	}
	page.Count = len(issues)
	// Labels and counts before the bucket: the map holds copies, and a copy
	// taken first would keep the empty ones.
	if err := fillPage(ctx, p.pool, issues); err != nil {
		return api.AssignedPage{}, err
	}
	for _, is := range issues {
		page.Assigned[is.Assignee] = append(page.Assigned[is.Assignee], is)
	}
	return page, nil
}

// buildAssignedQuery turns the assigned filter into SQL and its arguments.
// Rows come out in the lists' order, so one cursor helper pages both.
func buildAssignedQuery(assignees, statuses []string, rig string, wisps bool, cur *listCursor, limit int) (string, []any) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	where = append(where, "issues.assignee = ANY("+arg(assignees)+")", "issues.status = ANY("+arg(statuses)+")")
	// A seat's live work is a durable issue unless the caller says otherwise;
	// a hooked molecule root is a wisp, so a caller that wants it asks.
	if !wisps {
		where = append(where, "issues.ephemeral = false")
	}
	if rig != "" {
		where = append(where, "issues.rig = "+arg(rig))
	}
	if cur != nil {
		where = append(where, fmt.Sprintf("(issues.priority, issues.created_at, issues.id) > (%s, %s, %s)", arg(cur.Priority), arg(cur.CreatedAt), arg(cur.ID)))
	}
	q := "SELECT " + issueColumns + " FROM tracker.issues WHERE " + strings.Join(where, " AND ") +
		" ORDER BY priority, created_at, id LIMIT " + strconv.Itoa(limit+1)
	return q, args
}

// validateAssignedStatuses refuses a status the database does not know: the
// builtins plus the custom ones, as a write does, so a typo in a status is a
// 400 rather than an empty answer.
func (p *Postgres) validateAssignedStatuses(ctx context.Context, statuses []string) error {
	var custom []string
	for _, s := range statuses {
		if !isBuiltinStatus(s) {
			custom = append(custom, s)
		}
	}
	if len(custom) == 0 {
		return nil
	}
	rows, err := p.pool.Query(ctx, "SELECT name FROM tracker.custom_statuses WHERE name = ANY($1)", custom)
	if err != nil {
		return fmt.Errorf("store: assigned statuses: %w", err)
	}
	defer rows.Close()
	known := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("store: assigned statuses: %w", err)
		}
		known[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: assigned statuses: %w", err)
	}
	for _, s := range custom {
		if !known[s] {
			return api.Invalid("unknown status %q", s)
		}
	}
	return nil
}

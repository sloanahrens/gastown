package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
)

const (
	// defaultPageSize is a page when the caller sets no limit; it is a page,
	// never a cap: NextCursor says when more rows remain.
	defaultPageSize = 500
	maxPageSize     = 1000
)

// infraTypes are the issue types `bd list` leaves out unless asked.
var infraTypes = []string{"agent", "role", "message"}

func isInfraType(t string) bool { return slices.Contains(infraTypes, t) }

func pageSize(limit int) int {
	switch {
	case limit <= 0:
		return defaultPageSize
	case limit > maxPageSize:
		return maxPageSize
	}
	return limit
}

// listCursor is the sort key of the last row of a page: lists order by
// priority, creation time, then id, and the next page starts after it.
type listCursor struct {
	Priority  int       `json:"p"`
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

func (c listCursor) encode() string {
	// UTC, always: the database hands a time back in its session zone, and for
	// an early date that zone's offset has seconds (-05:50:36 for year 1), which
	// the RFC 3339 text in the cursor cannot carry. A cursor that decodes to a
	// different instant makes a page repeat forever.
	c.CreatedAt = c.CreatedAt.UTC()
	b, _ := json.Marshal(c) // three plain fields; Marshal cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeListCursor reads a cursor; "" is the first page and returns nil.
func decodeListCursor(s string) (*listCursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, api.Invalid("invalid cursor")
	}
	var c listCursor
	if err := json.Unmarshal(b, &c); err != nil || c.ID == "" {
		return nil, api.Invalid("invalid cursor")
	}
	return &c, nil
}

// buildListWhere turns a filter and cursor into the WHERE clause a list and a
// count share, and its arguments. Only placeholders carry values, so no
// filter text reaches the query string.
func buildListWhere(f api.ListFilter, cur *listCursor) (string, []any, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	where = append(where, "issues.ephemeral = "+arg(f.Ephemeral))
	if !f.IncludeInfra && f.IssueType == "" {
		where = append(where, "issues.issue_type <> ALL("+arg(infraTypes)+")")
	}
	switch {
	case len(f.Statuses) == 0:
		where = append(where, "issues.status <> 'closed'")
	case slices.Contains(f.Statuses, "all"):
	default:
		where = append(where, "issues.status = ANY("+arg(f.Statuses)+")")
	}
	if labels := uniqueStrings(f.Labels); len(labels) > 0 {
		where = append(where, fmt.Sprintf("(SELECT count(*) FROM tracker.labels l WHERE l.issue_id = issues.id AND l.label = ANY(%s)) = %d", arg(labels), len(labels)))
	}
	if f.Priority != nil {
		where = append(where, "issues.priority = "+arg(*f.Priority))
	}
	if f.Parent != "" {
		where = append(where, "issues.parent_id = "+arg(f.Parent))
	}
	if f.NoAssignee && f.Assignee != "" {
		return "", nil, api.Invalid("assignee and no_assignee cannot both be set")
	}
	if f.Assignee != "" {
		where = append(where, "issues.assignee = "+arg(f.Assignee))
	}
	if f.NoAssignee {
		where = append(where, "issues.assignee = ''")
	}
	if f.IssueType != "" {
		where = append(where, "issues.issue_type = "+arg(f.IssueType))
	}
	if f.Rig != "" {
		where = append(where, "issues.rig = "+arg(f.Rig))
	}
	if len(f.Rigs) > 0 {
		where = append(where, "issues.rig = ANY("+arg(f.Rigs)+")")
	}
	if len(f.IDs) > 0 {
		where = append(where, "issues.id = ANY("+arg(f.IDs)+")")
	}
	if !f.ClosedAfter.IsZero() {
		where = append(where, "issues.closed_at >= "+arg(f.ClosedAfter))
	}
	if !f.ClosedBefore.IsZero() {
		// NULL < anything is NULL, so an issue that is not closed never
		// matches an upper bound either.
		where = append(where, "issues.closed_at < "+arg(f.ClosedBefore))
	}
	if !f.CreatedAfter.IsZero() {
		where = append(where, "issues.created_at >= "+arg(f.CreatedAfter))
	}
	if !f.CreatedBefore.IsZero() {
		where = append(where, "issues.created_at < "+arg(f.CreatedBefore))
	}
	if cur != nil {
		where = append(where, fmt.Sprintf("(issues.priority, issues.created_at, issues.id) > (%s, %s, %s)", arg(cur.Priority), arg(cur.CreatedAt), arg(cur.ID)))
	}
	return strings.Join(where, " AND "), args, nil
}

// buildListQuery is one page of the list: the shared WHERE clause, then the
// list's order and one row past the page so NextCursor knows whether more
// remain.
func buildListQuery(f api.ListFilter, cur *listCursor, limit int) (string, []any, error) {
	where, args, err := buildListWhere(f, cur)
	if err != nil {
		return "", nil, err
	}
	q := "SELECT " + issueColumns + " FROM tracker.issues WHERE " + where +
		" ORDER BY priority, created_at, id LIMIT " + strconv.Itoa(limit+1)
	return q, args, nil
}

// buildCountQuery counts the whole match the list's WHERE clause selects: the
// same conditions, with no order and no limit to drop a row from the total.
func buildCountQuery(f api.ListFilter) (string, []any, error) {
	where, args, err := buildListWhere(f, nil)
	if err != nil {
		return "", nil, err
	}
	return "SELECT count(*) FROM tracker.issues WHERE " + where, args, nil
}

// rigScope is f.Rigs split into the rigs the tracker holds and the rigs it does
// not. A rig it does not hold is never answered with no issues, which would
// read as a rig whose queue emptied: the list reports it in RigErrors and the
// count refuses it.
func (p *Postgres) rigScope(ctx context.Context, f api.ListFilter) (held, unknown []string, err error) {
	if len(f.Rigs) == 0 {
		return nil, nil, nil
	}
	if f.Rig != "" {
		return nil, nil, api.Invalid("rig and rigs cannot both be set")
	}
	rigs := uniqueStrings(f.Rigs)
	if len(rigs) == 0 {
		return nil, nil, api.Invalid("rigs holds no rig")
	}
	rows, err := p.pool.Query(ctx, "SELECT prefix FROM tracker.rigs WHERE prefix = ANY($1)", rigs)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list rigs: %w", err)
	}
	defer rows.Close()
	known := map[string]bool{}
	for rows.Next() {
		var rig string
		if err := rows.Scan(&rig); err != nil {
			return nil, nil, fmt.Errorf("store: list rigs: %w", err)
		}
		known[rig] = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list rigs: %w", err)
	}
	for _, rig := range rigs {
		if known[rig] {
			held = append(held, rig)
		} else {
			unknown = append(unknown, rig)
		}
	}
	return held, unknown, nil
}

// unknownRigErrors is the RigErrors of a list: each rig it does not hold, with
// why. nil when every rig asked for is held.
func unknownRigErrors(unknown []string) map[string]string {
	if len(unknown) == 0 {
		return nil
	}
	out := make(map[string]string, len(unknown))
	for _, rig := range unknown {
		out[rig] = "unknown rig " + rig
	}
	return out
}

// ListIssues returns one page of issues matching f, ordered by priority,
// creation time and id, with comment and dependency counts filled in.
func (p *Postgres) ListIssues(ctx context.Context, f api.ListFilter) (api.IssuePage, error) {
	held, unknown, err := p.rigScope(ctx, f)
	if err != nil {
		return api.IssuePage{}, err
	}
	refused := unknownRigErrors(unknown)
	if len(held) == 0 && len(refused) > 0 {
		// Every rig asked for is unknown, so there is nothing to read.
		return api.IssuePage{RigErrors: refused}, nil
	}
	f.Rigs = held
	cur, err := decodeListCursor(f.Cursor)
	if err != nil {
		return api.IssuePage{}, err
	}
	size := pageSize(f.Limit)
	q, args, err := buildListQuery(f, cur, size)
	if err != nil {
		return api.IssuePage{}, err
	}
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return api.IssuePage{}, fmt.Errorf("store: list issues: %w", err)
	}
	defer rows.Close()
	var issues []api.Issue
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return api.IssuePage{}, fmt.Errorf("store: list issues: %w", err)
		}
		issues = append(issues, is)
	}
	if err := rows.Err(); err != nil {
		return api.IssuePage{}, fmt.Errorf("store: list issues: %w", err)
	}
	page := api.IssuePage{Issues: issues, RigErrors: refused}
	if len(issues) > size {
		page.Issues = issues[:size]
		last := page.Issues[size-1]
		page.NextCursor = listCursor{Priority: last.Priority, CreatedAt: last.CreatedAt, ID: last.ID}.encode()
	}
	if err := fillPage(ctx, p.pool, page.Issues); err != nil {
		return api.IssuePage{}, err
	}
	return page, nil
}

// CountIssues counts every issue f matches.
func (p *Postgres) CountIssues(ctx context.Context, f api.ListFilter) (int, error) {
	if err := api.ValidateCount(f); err != nil {
		return 0, err
	}
	held, unknown, err := p.rigScope(ctx, f)
	if err != nil {
		return 0, err
	}
	if len(unknown) > 0 {
		return 0, api.Invalid("unknown rig %s", strings.Join(unknown, ", "))
	}
	f.Rigs = held
	q, args, err := buildCountQuery(f)
	if err != nil {
		return 0, err
	}
	var n int
	if err := p.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count issues: %w", err)
	}
	return n, nil
}

// fillPage adds labels and the three counts to the issues of a page.
func fillPage(ctx context.Context, q querier, issues []api.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	ids := make([]string, len(issues))
	for i, is := range issues {
		ids[i] = is.ID
	}
	labels, err := loadLabels(ctx, q, ids)
	if err != nil {
		return err
	}
	counts := map[string]*[3]int{}
	for _, c := range []struct {
		slot int
		sql  string
	}{
		{0, "SELECT issue_id, count(*) FROM tracker.comments WHERE issue_id = ANY($1) GROUP BY 1"},
		{1, "SELECT issue_id, count(*) FROM tracker.dependencies WHERE issue_id = ANY($1) GROUP BY 1"},
		{2, "SELECT depends_on, count(*) FROM tracker.dependencies WHERE depends_on = ANY($1) GROUP BY 1"},
	} {
		rows, err := q.Query(ctx, c.sql, ids)
		if err != nil {
			return fmt.Errorf("store: list counts: %w", err)
		}
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return fmt.Errorf("store: list counts: %w", err)
			}
			if counts[id] == nil {
				counts[id] = &[3]int{}
			}
			counts[id][c.slot] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: list counts: %w", err)
		}
	}
	for i := range issues {
		issues[i].Labels = labels[issues[i].ID]
		if c := counts[issues[i].ID]; c != nil {
			issues[i].CommentCount, issues[i].DependencyCount, issues[i].DependentCount = c[0], c[1], c[2]
		}
	}
	return nil
}

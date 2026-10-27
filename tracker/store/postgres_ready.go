package store

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/tracker/api"
)

// A link blocks when its type is in blockingTypes and its target is neither
// closed nor pinned (resolvedStatuses); blocked issues pass that down to their
// children. Every query that asks "what blocks" builds its SQL from these two
// lists, so ready, blocked, BlockedBy and the close fence cannot disagree.
var (
	blockingTypes    = []string{"blocks", "conditional-blocks"}
	resolvedStatuses = []string{"closed", "pinned"}
)

// sqlList renders a list of constants as a SQL list literal: ('a', 'b').
func sqlList(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = "'" + v + "'"
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}

var (
	// blocking is the SQL list of types that block.
	blocking = sqlList(blockingTypes)
	// unresolved is the condition, on an alias t, that a target still blocks.
	unresolved = "t.status NOT IN " + sqlList(resolvedStatuses)
)

// blockedCTE defines `blocked` for the whole database: issues with an
// unresolved blocker, plus, transitively, the children of any blocked issue (a
// blocked parent blocks its children). Links of other types, and external
// targets, never block.
var blockedCTE = `direct AS (
  SELECT DISTINCT d.issue_id AS id FROM tracker.dependencies d
  JOIN tracker.issues t ON t.id = d.depends_on
  WHERE d.type IN ` + blocking + ` AND ` + unresolved + `
), blocked AS (
  SELECT id FROM direct
  UNION
  SELECT c.id FROM tracker.issues c JOIN blocked b ON c.parent_id = b.id
)`

// moleculeBlockedCTE defines `blocked` inside a molecule: only the links
// between its own steps count, and what blocks the root or lies outside does
// not reach in. A molecule's root waits on its work bead while its steps are
// still done in order, and gt's step walker depends on that.
var moleculeBlockedCTE = `direct AS (
  SELECT DISTINCT d.issue_id AS id FROM tracker.dependencies d
  JOIN tracker.issues t ON t.id = d.depends_on
  WHERE d.type IN ` + blocking + ` AND ` + unresolved + `
    AND d.issue_id IN (SELECT id FROM scope) AND d.depends_on IN (SELECT id FROM scope)
), blocked AS (
  SELECT id FROM direct
  UNION
  SELECT c.id FROM tracker.issues c JOIN blocked b ON c.parent_id = b.id WHERE c.id IN (SELECT id FROM scope)
)`

// scopeCTE defines `scope`: every descendant of the molecule root $n, wisps
// included, not the root itself.
func scopeCTE(rootArg string) string {
	return `scope AS (
  SELECT id FROM tracker.issues WHERE parent_id = ` + rootArg + `
  UNION
  SELECT c.id FROM tracker.issues c JOIN scope s ON c.parent_id = s.id
)`
}

// readyExcludedTypes are the types the whole-database ready board leaves out,
// as bd's does: the infrastructure types, and the kinds of bead that are not
// work to pick up (merge requests, gates, molecule roots, rig identity beads).
// Inside one molecule only the infrastructure types are left out.
var readyExcludedTypes = append(slices.Clone(infraTypes), "merge-request", "gate", "molecule", "rig")

// buildReadyQuery builds the ready query (blocked=false) or the blocked one.
// Ready is open (or deferred and woken), unblocked, not ephemeral (unless scoped to a
// molecule) and not an infrastructure type; blocked is not-closed and blocked.
// ReadyFilter.Gated has no effect: no gate rows exist.
func buildReadyQuery(f api.ReadyFilter, cur *listCursor, limit int, blocked bool) (string, []any) {
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	var ctes string
	var where []string
	if f.Molecule != "" {
		ctes = "WITH RECURSIVE " + scopeCTE(arg(f.Molecule)) + ", " + moleculeBlockedCTE
		where = append(where, "issues.id IN (SELECT id FROM scope)")
	} else {
		ctes = "WITH RECURSIVE " + blockedCTE
		where = append(where, "issues.ephemeral = false")
	}
	if blocked {
		where = append(where, "issues.status <> 'closed'", "issues.id IN (SELECT id FROM blocked)")
	} else {
		// An open issue is ready unless it is deferred into the future; a
		// deferred issue wakes by itself once its time has passed, with no
		// sweep writing to the table.
		where = append(where, "issues.id NOT IN (SELECT id FROM blocked)",
			"((issues.status = 'open' AND (issues.defer_until IS NULL OR issues.defer_until <= now())) OR (issues.status = 'deferred' AND issues.defer_until IS NOT NULL AND issues.defer_until <= now()))")
	}
	if f.Molecule == "" {
		where = append(where, "issues.issue_type <> ALL("+arg(readyExcludedTypes)+")")
	} else {
		where = append(where, "issues.issue_type <> ALL("+arg(infraTypes)+")")
	}
	if f.Rig != "" {
		where = append(where, "issues.rig = "+arg(f.Rig))
	}
	if f.Parent != "" {
		where = append(where, "issues.parent_id = "+arg(f.Parent))
	}
	if f.Unassigned {
		where = append(where, "issues.assignee = ''")
	}
	if len(f.ExcludeLabels) > 0 {
		where = append(where, "NOT EXISTS (SELECT 1 FROM tracker.labels l WHERE l.issue_id = issues.id AND l.label = ANY("+arg(f.ExcludeLabels)+"))")
	}
	if len(f.ExcludeTypes) > 0 {
		where = append(where, "issues.issue_type <> ALL("+arg(f.ExcludeTypes)+")")
	}
	if cur != nil {
		where = append(where, fmt.Sprintf("(issues.priority, issues.created_at, issues.id) > (%s, %s, %s)", arg(cur.Priority), arg(cur.CreatedAt), arg(cur.ID)))
	}
	q := ctes + " SELECT " + issueColumns + " FROM tracker.issues WHERE " + strings.Join(where, " AND ") +
		" ORDER BY priority, created_at, id LIMIT " + strconv.Itoa(limit+1)
	return q, args
}

// Ready returns the open, unblocked work matching f, ordered by priority,
// creation time and id, one page at a time.
func (p *Postgres) Ready(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error) {
	return readyPage(ctx, p.pool, f, false)
}

// Blocked returns the not-closed issues held by an open blocker (or by a
// blocked parent) matching f, each with its direct open blockers in BlockedBy.
func (p *Postgres) Blocked(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error) {
	page, err := readyPage(ctx, p.pool, f, true)
	if err != nil {
		return api.IssuePage{}, err
	}
	return page, fillBlockedBy(ctx, p.pool, page.Issues)
}

func readyPage(ctx context.Context, q querier, f api.ReadyFilter, blocked bool) (api.IssuePage, error) {
	cur, err := decodeListCursor(f.Cursor)
	if err != nil {
		return api.IssuePage{}, err
	}
	size := pageSize(f.Limit)
	sql, args := buildReadyQuery(f, cur, size, blocked)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return api.IssuePage{}, fmt.Errorf("store: ready: %w", err)
	}
	defer rows.Close()
	var issues []api.Issue
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return api.IssuePage{}, fmt.Errorf("store: ready: %w", err)
		}
		issues = append(issues, is)
	}
	if err := rows.Err(); err != nil {
		return api.IssuePage{}, fmt.Errorf("store: ready: %w", err)
	}
	page := api.IssuePage{Issues: issues}
	if len(issues) > size {
		page.Issues = issues[:size]
		last := page.Issues[size-1]
		page.NextCursor = listCursor{Priority: last.Priority, CreatedAt: last.CreatedAt, ID: last.ID}.encode()
	}
	return page, fillPage(ctx, q, page.Issues)
}

// fillBlockedBy sets each issue's BlockedBy to its direct open blockers.
func fillBlockedBy(ctx context.Context, q querier, issues []api.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	ids := make([]string, len(issues))
	at := map[string]int{}
	for i, is := range issues {
		ids[i], at[is.ID] = is.ID, i
	}
	rows, err := q.Query(ctx, `SELECT d.issue_id, d.depends_on FROM tracker.dependencies d
JOIN tracker.issues t ON t.id = d.depends_on
WHERE d.issue_id = ANY($1) AND d.type IN `+blocking+` AND `+unresolved+` ORDER BY d.issue_id, d.depends_on`, ids)
	if err != nil {
		return fmt.Errorf("store: blocked by: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, by string
		if err := rows.Scan(&id, &by); err != nil {
			return fmt.Errorf("store: blocked by: %w", err)
		}
		issues[at[id]].BlockedBy = append(issues[at[id]].BlockedBy, by)
	}
	return rows.Err()
}

// ChildrenOf returns the direct children of each of parents, any status and
// wisps included, ordered by creation, keyed by parent. A parent with no
// children, or that does not exist, is absent.
func (p *Postgres) ChildrenOf(ctx context.Context, parents []string) (map[string][]api.Issue, error) {
	out := map[string][]api.Issue{}
	parents = uniqueStrings(parents)
	if len(parents) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, "SELECT "+issueColumns+" FROM tracker.issues WHERE parent_id = ANY($1) ORDER BY created_at, id", parents)
	if err != nil {
		return nil, fmt.Errorf("store: children: %w", err)
	}
	defer rows.Close()
	var all []api.Issue
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("store: children: %w", err)
		}
		all = append(all, is)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: children: %w", err)
	}
	ids := make([]string, len(all))
	for i, is := range all {
		ids[i] = is.ID
	}
	labels, err := loadLabels(ctx, p.pool, ids)
	if err != nil {
		return nil, err
	}
	for _, is := range all {
		is.Labels = labels[is.ID]
		out[is.Parent] = append(out[is.Parent], is)
	}
	return out, nil
}

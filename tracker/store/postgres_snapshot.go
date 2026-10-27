package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// TownSnapshot reads one page of the town's durable issues, their blocked
// verdicts and the counts over the whole set inside one repeatable-read,
// read-only transaction, so a writer that commits while the read runs is
// visible in no part of the answer. See api.TownSnapshot for what one page
// does and does not promise about a walk over several pages.
func (p *Postgres) TownSnapshot(ctx context.Context, req api.SnapshotRequest) (api.TownSnapshot, error) {
	cur, err := decodeListCursor(req.Cursor)
	if err != nil {
		return api.TownSnapshot{}, err
	}
	size := pageSize(req.Limit)
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return api.TownSnapshot{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	issues, truncated, err := snapshotPage(ctx, tx, cur, size)
	if err != nil {
		return api.TownSnapshot{}, err
	}
	blocked, err := blockedIssues(ctx, tx)
	if err != nil {
		return api.TownSnapshot{}, err
	}
	counts, err := snapshotCounts(ctx, tx, blocked)
	if err != nil {
		return api.TownSnapshot{}, err
	}
	if err := fillPage(ctx, tx, issues); err != nil {
		return api.TownSnapshot{}, err
	}
	if err := fillBlockedBy(ctx, tx, issues); err != nil {
		return api.TownSnapshot{}, err
	}

	snap := api.TownSnapshot{
		Issues:    make([]api.SnapshotIssue, len(issues)),
		Counts:    counts,
		Truncated: truncated,
	}
	for i, is := range issues {
		_, snap.Issues[i].Blocked = blocked[is.ID]
		snap.Issues[i].Issue = is
	}
	if truncated {
		last := issues[len(issues)-1]
		snap.NextCursor = listCursor{Priority: last.Priority, CreatedAt: last.CreatedAt, ID: last.ID}.encode()
	}
	// now() is the transaction's start, so it is this page's moment however
	// late in the read it is asked for.
	if err := tx.QueryRow(ctx, "SELECT now()").Scan(&snap.At); err != nil {
		return api.TownSnapshot{}, fmt.Errorf("store: snapshot at: %w", err)
	}
	return snap, nil
}

// snapshotPage reads one page of the durable issues in list order, plus one
// row to learn whether the page was cut.
func snapshotPage(ctx context.Context, q querier, cur *listCursor, size int) (issues []api.Issue, truncated bool, err error) {
	sql, args := buildSnapshotQuery(cur, size)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: snapshot page: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return nil, false, fmt.Errorf("store: snapshot page: %w", err)
		}
		issues = append(issues, is)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: snapshot page: %w", err)
	}
	if len(issues) > size {
		return issues[:size], true, nil
	}
	return issues, false, nil
}

// buildSnapshotQuery selects one page of the durable issues by the lists'
// sort key, so the list cursor resumes it.
func buildSnapshotQuery(cur *listCursor, limit int) (string, []any) {
	where := "NOT ephemeral"
	var args []any
	if cur != nil {
		args = append(args, cur.Priority, cur.CreatedAt, cur.ID)
		where += " AND (priority, created_at, id) > ($1, $2, $3)"
	}
	return "SELECT " + issueColumns + " FROM tracker.issues WHERE " + where +
		" ORDER BY priority, created_at, id LIMIT " + strconv.Itoa(limit+1), args
}

// blockedIssues returns the id and rig of every blocked durable issue from one
// recursive read, so a page's verdicts and the blocked counts come from the
// same set and the recursive query runs once per snapshot.
func blockedIssues(ctx context.Context, q querier) (map[string]string, error) {
	rows, err := q.Query(ctx, "WITH RECURSIVE "+blockedCTE+
		" SELECT i.id, i.rig FROM blocked b JOIN tracker.issues i ON i.id = b.id WHERE NOT i.ephemeral")
	if err != nil {
		return nil, fmt.Errorf("store: snapshot blocked: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, rig string
		if err := rows.Scan(&id, &rig); err != nil {
			return nil, fmt.Errorf("store: snapshot blocked: %w", err)
		}
		out[id] = rig
	}
	return out, rows.Err()
}

// snapshotCounts counts the durable issues by rig and status in one read and
// derives the totals and the blocked counts from them. The counts cover every
// issue, not the page.
func snapshotCounts(ctx context.Context, q querier, blocked map[string]string) (api.SnapshotCounts, error) {
	counts := api.SnapshotCounts{
		ByStatus:    map[string]int{},
		ByRig:       map[string]int{},
		BlockedRigs: map[string]int{},
	}
	rows, err := q.Query(ctx, "SELECT rig, status, count(*) FROM tracker.issues WHERE NOT ephemeral GROUP BY rig, status")
	if err != nil {
		return api.SnapshotCounts{}, fmt.Errorf("store: snapshot counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rig, status string
		var n int
		if err := rows.Scan(&rig, &status, &n); err != nil {
			return api.SnapshotCounts{}, fmt.Errorf("store: snapshot counts: %w", err)
		}
		counts.ByRig[rig] += n
		counts.ByStatus[status] += n
		counts.Total += n
	}
	if err := rows.Err(); err != nil {
		return api.SnapshotCounts{}, fmt.Errorf("store: snapshot counts: %w", err)
	}
	for _, rig := range blocked {
		counts.Blocked++
		counts.BlockedRigs[rig]++
	}
	return counts, nil
}

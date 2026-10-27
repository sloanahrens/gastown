package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// fenceLimit is how many offending ids a refusal names.
const fenceLimit = 5

// openChildren returns up to fenceLimit children of id that are not closed
// and not in closing, the set being closed together with id.
func openChildren(ctx context.Context, q querier, id string, closing []string) ([]string, error) {
	return stringsFrom(ctx, q, "SELECT id FROM tracker.issues WHERE parent_id = $1 AND status <> 'closed' AND id <> ALL($2) ORDER BY id LIMIT "+fmt.Sprint(fenceLimit), id, closing)
}

// openBlockers returns up to fenceLimit issues id depends on through a
// blocking link that are not closed and not in closing, the set being closed
// together with id. Only `blocks` links count here; the parent-child rule
// belongs to ready/blocked, not to closing.
func openBlockers(ctx context.Context, q querier, id string, closing []string) ([]string, error) {
	return stringsFrom(ctx, q, `SELECT d.depends_on FROM tracker.dependencies d
JOIN tracker.issues t ON t.id = d.depends_on
WHERE d.issue_id = $1 AND d.type IN `+blocking+` AND `+unresolved+` AND t.id <> ALL($2) ORDER BY d.depends_on LIMIT `+fmt.Sprint(fenceLimit), id, closing)
}

func stringsFrom(ctx context.Context, q querier, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// closeRow closes one locked, not-yet-closed issue in tx. Unless force, it
// refuses while the issue has open children or an open blocker, and says
// which; members of closing (the set closed in the same call) do not count.
// fromClose is true for the close verb and false for an update to status
// closed: only the close verb also refuses a pinned issue and an issue
// assigned to someone other than the actor, as bd does.
func closeRow(ctx context.Context, tx pgx.Tx, id, reason string, force bool, actor string, closing []string, fromClose bool) error {
	if !force {
		if fromClose {
			var status, assignee string
			if err := tx.QueryRow(ctx, "SELECT status, assignee FROM tracker.issues WHERE id = $1", id).Scan(&status, &assignee); err != nil {
				return fmt.Errorf("store: read %s: %w", id, err)
			}
			if status == "pinned" {
				return api.Conflict("%s is pinned (release it, or force)", id)
			}
			if assignee != "" && assignee != actor {
				return api.Conflict("%s is assigned to %s, not to %s (release it, or force)", id, assignee, actor)
			}
		}
		if kids, err := openChildren(ctx, tx, id, closing); err != nil {
			return err
		} else if len(kids) > 0 {
			return api.Conflict("%s has open children: %s (close them first, or force)", id, strings.Join(kids, ", "))
		}
		if blockers, err := openBlockers(ctx, tx, id, closing); err != nil {
			return err
		} else if len(blockers) > 0 {
			return api.Conflict("%s is blocked by open issues: %s (close them first, or force)", id, strings.Join(blockers, ", "))
		}
	}
	if _, err := tx.Exec(ctx, "UPDATE tracker.issues SET status = 'closed', closed_at = now(), close_reason = $2, updated_at = now() WHERE id = $1", id, reason); err != nil {
		return fmt.Errorf("store: close %s: %w", id, err)
	}
	return appendEvent(ctx, tx, "closed", id, actor, nil, nil, &reason)
}

// CloseIssues closes every issue in ids with reason, or none: it locks them
// all, refuses if any is missing, and closes the ones not already closed. A
// closed issue is skipped, so closing twice is not an error. Unless force, an
// issue with open children or an open blocker refuses and nothing is closed.
func (p *Postgres) CloseIssues(ctx context.Context, ids []string, reason string, force bool) error {
	if len(uniqueStrings(ids)) == 0 {
		return nil
	}
	actor := api.ActorFrom(ctx)
	return p.inTx(ctx, func(tx pgx.Tx) error { return closeAllInTx(ctx, tx, ids, reason, force, actor) })
}

// closeAllInTx closes every id not already closed, or none: it locks them all
// first, so a missing id refuses the set before anything changes.
func closeAllInTx(ctx context.Context, tx pgx.Tx, ids []string, reason string, force bool, actor string) error {
	ids = uniqueStrings(ids)
	if len(ids) == 0 {
		return nil
	}
	status, err := lockStatuses(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if status[id] == "closed" {
			continue
		}
		if err := closeRow(ctx, tx, id, reason, force, actor, ids, true); err != nil {
			return err
		}
	}
	return nil
}

// lockStatuses locks ids in id order (a fixed order, so concurrent callers
// cannot deadlock) and returns their statuses; a missing id is not found.
func lockStatuses(ctx context.Context, tx pgx.Tx, ids []string) (map[string]string, error) {
	rows, err := tx.Query(ctx, "SELECT id, status FROM tracker.issues WHERE id = ANY($1) ORDER BY id FOR UPDATE", ids)
	if err != nil {
		return nil, fmt.Errorf("store: lock issues: %w", err)
	}
	status := map[string]string{}
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: lock issues: %w", err)
		}
		status[id] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: lock issues: %w", err)
	}
	for _, id := range ids {
		if _, ok := status[id]; !ok {
			return nil, api.NotFound(id)
		}
	}
	return status, nil
}

// DeleteIssues deletes every issue in ids, or none. Their labels, comments and
// outgoing links go with them (the schema's ON DELETE CASCADE on issue_id),
// links pointing at them are removed here, children are left without a parent
// but are not deleted, and their events remain.
func (p *Postgres) DeleteIssues(ctx context.Context, ids []string) error {
	ids = uniqueStrings(ids)
	if len(ids) == 0 {
		return nil
	}
	actor := api.ActorFrom(ctx)
	return p.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockStatuses(ctx, tx, ids); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM tracker.dependencies WHERE depends_on = ANY($1)", ids); err != nil {
			return fmt.Errorf("store: delete links: %w", err)
		}
		if _, err := tx.Exec(ctx, "UPDATE tracker.issues SET parent_id = NULL WHERE parent_id = ANY($1) AND id <> ALL($1)", ids); err != nil {
			return fmt.Errorf("store: orphan children: %w", err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM tracker.issues WHERE id = ANY($1)", ids); err != nil {
			return fmt.Errorf("store: delete: %w", err)
		}
		for _, id := range ids {
			if err := appendEvent(ctx, tx, "deleted", id, actor, nil, nil, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

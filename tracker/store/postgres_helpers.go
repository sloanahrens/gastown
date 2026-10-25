package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/steveyegge/gastown/tracker/api"
)

// SQLSTATEs the store maps to API errors.
const (
	sqlUniqueViolation     = "23505"
	sqlForeignKeyViolation = "23503"
)

// issueColumns is the SELECT list scanIssue reads, in order.
const issueColumns = `id, title, description, design, notes, acceptance_criteria, status, priority,
issue_type, assignee, owner, created_by, created_at, updated_at, closed_at, defer_until, close_reason, coalesce(parent_id, ''),
ephemeral, wisp_type, event_kind, coalesce(payload::text, ''), hook_bead, agent_state, last_activity`

// scanIssue reads one row of issueColumns. Labels are loaded separately.
func scanIssue(row pgx.Row) (api.Issue, error) {
	var is api.Issue
	var eventKind string
	err := row.Scan(&is.ID, &is.Title, &is.Description, &is.Design, &is.Notes, &is.AcceptanceCriteria, &is.Status, &is.Priority,
		&is.Type, &is.Assignee, &is.Owner, &is.CreatedBy, &is.CreatedAt, &is.UpdatedAt, &is.ClosedAt, &is.DeferUntil, &is.CloseReason, &is.Parent,
		&is.Ephemeral, &is.WispType, &eventKind, &is.Payload, &is.HookBead, &is.AgentState, &is.LastActivity)
	is.EventKind = eventKind
	return is, err
}

// querier is what the pool and a transaction share.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// loadLabels returns the sorted labels of each of ids that has any.
func loadLabels(ctx context.Context, q querier, ids []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, "SELECT issue_id, label FROM tracker.labels WHERE issue_id = ANY($1) ORDER BY issue_id, label", ids)
	if err != nil {
		return nil, fmt.Errorf("store: load labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, fmt.Errorf("store: load labels: %w", err)
		}
		out[id] = append(out[id], label)
	}
	return out, rows.Err()
}

// appendEvent adds one row to the append-only events feed inside tx. Writers
// call it in the same transaction as the change it records.
func appendEvent(ctx context.Context, tx pgx.Tx, op, issueID, actor string, oldValue, newValue, comment *string) error {
	_, err := tx.Exec(ctx, "INSERT INTO tracker.events (op, issue_id, actor, old_value, new_value, comment) VALUES ($1, $2, $3, $4, $5, $6)",
		op, issueID, actor, oldValue, newValue, comment)
	if err != nil {
		return fmt.Errorf("store: append %s event for %s: %w", op, issueID, err)
	}
	return nil
}

// inTx runs fn in a transaction: committed when fn returns nil, rolled back
// otherwise. The rollback uses a context that outlives ctx's cancellation so
// a canceled request does not leave the transaction open.
func (p *Postgres) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// pgCode returns the SQLSTATE and constraint name of err, if it is a
// Postgres error.
func pgCode(err error) (code, constraint string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.ConstraintName
	}
	return "", ""
}

// uniqueStrings returns ss sorted with duplicates and empty strings removed.
func uniqueStrings(ss []string) []string {
	out := slices.DeleteFunc(slices.Clone(ss), func(s string) bool { return s == "" })
	slices.Sort(out)
	return slices.Compact(out)
}

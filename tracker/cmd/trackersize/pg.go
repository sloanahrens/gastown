package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// pgQuerier is the Querier a run uses: one read of the tracker database. Unit
// tests stand a fake in for it, so it holds no logic of its own.
type pgQuerier struct {
	dsn string
}

// sizesSQL totals the issues per issue-id prefix. octet_length counts the bytes
// of each text column; the jsonb columns (payload, metadata) are not text and
// are left out. sum() is numeric, so it is cast back to the integer type the
// reader scans.
const sizesSQL = `
SELECT rig AS prefix,
       count(*) AS row_count,
       coalesce(sum(
             octet_length(id)
           + octet_length(rig)
           + octet_length(issue_type)
           + octet_length(status)
           + octet_length(title)
           + octet_length(description)
           + octet_length(design)
           + octet_length(acceptance_criteria)
           + octet_length(notes)
           + octet_length(assignee)
           + octet_length(owner)
           + octet_length(created_by)
           + octet_length(close_reason)
           + octet_length(wisp_type)
           + octet_length(event_kind)
           + octet_length(hook_bead)
           + octet_length(agent_state)
           + coalesce(octet_length(parent_id), 0)
       ), 0)::bigint AS byte_count
  FROM tracker.issues
 GROUP BY rig
 ORDER BY rig`

// Sizes connects to the tracker, reads the totals and closes the connection
// again: a run is one read, so the connection lives for the call.
//
// The DSN carries a password, so no message here repeats it. pgconn's connect
// error names the user and the database only.
func (q pgQuerier) Sizes(ctx context.Context) ([]PrefixSize, error) {
	conn, err := pgx.Connect(ctx, q.dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to the tracker: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, sizesSQL)
	if err != nil {
		return nil, fmt.Errorf("query issue sizes: %w", err)
	}
	defer rows.Close()
	var sizes []PrefixSize
	for rows.Next() {
		var s PrefixSize
		if err := rows.Scan(&s.Prefix, &s.Rows, &s.Bytes); err != nil {
			return nil, fmt.Errorf("read a size row: %w", err)
		}
		sizes = append(sizes, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the size rows: %w", err)
	}
	return sizes, nil
}

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// Export streams rig's issues, wisps included, then the labels, dependencies
// and comments of those issues. The four reads run one after another in one
// repeatable-read, read-only transaction, so a write that commits during the
// export is in none of it. Rows are scanned and emitted one at a time: the
// rig is never held in memory. Ids are ordered by bytes, not by the database's
// locale, so two exports of the same rig are the same stream on any host.
func (p *Postgres) Export(ctx context.Context, rig string, emit func(api.ExportRecord) error) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("store: export: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	reads := []struct {
		what  string
		query string
		scan  func(pgx.Rows) (api.ExportRecord, error)
	}{
		{"issues", "SELECT " + issueColumns + " FROM tracker.issues WHERE rig = $1 ORDER BY id COLLATE \"C\"", scanExportIssue},
		{"labels", "SELECT l.issue_id, l.label FROM tracker.labels l JOIN tracker.issues i ON i.id = l.issue_id WHERE i.rig = $1 ORDER BY l.issue_id COLLATE \"C\", l.label COLLATE \"C\"", scanExportLabel},
		{"dependencies", "SELECT d.issue_id, d.depends_on, d.type, coalesce(d.metadata::text, ''), d.thread_id, d.created_by, d.created_at FROM tracker.dependencies d JOIN tracker.issues i ON i.id = d.issue_id WHERE i.rig = $1 ORDER BY d.issue_id COLLATE \"C\", d.created_at, d.id", scanExportDependency},
		{"comments", "SELECT c.id::text, c.issue_id, c.author, c.text, c.created_at FROM tracker.comments c JOIN tracker.issues i ON i.id = c.issue_id WHERE i.rig = $1 ORDER BY c.issue_id COLLATE \"C\", c.created_at, c.id", scanExportComment},
	}
	for _, r := range reads {
		if err := exportRows(ctx, tx, r.what, r.query, rig, r.scan, emit); err != nil {
			return err
		}
	}
	return nil
}

// exportRows runs one export read and emits a record per row. An error from
// emit is returned as it is, so the caller sees the write that failed.
func exportRows(ctx context.Context, tx pgx.Tx, what, query, rig string, scan func(pgx.Rows) (api.ExportRecord, error), emit func(api.ExportRecord) error) error {
	rows, err := tx.Query(ctx, query, rig)
	if err != nil {
		return fmt.Errorf("store: export %s: %w", what, err)
	}
	defer rows.Close()
	for rows.Next() {
		rec, err := scan(rows)
		if err != nil {
			return fmt.Errorf("store: export %s: %w", what, err)
		}
		if err := emit(rec); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: export %s: %w", what, err)
	}
	return nil
}

func scanExportIssue(rows pgx.Rows) (api.ExportRecord, error) {
	is, err := scanIssue(rows)
	if err != nil {
		return api.ExportRecord{}, err
	}
	return api.ExportRecord{Type: api.RecordIssue, Issue: &is}, nil
}

func scanExportLabel(rows pgx.Rows) (api.ExportRecord, error) {
	var l api.ExportLabel
	if err := rows.Scan(&l.IssueID, &l.Label); err != nil {
		return api.ExportRecord{}, err
	}
	return api.ExportRecord{Type: api.RecordLabel, Label: &l}, nil
}

func scanExportDependency(rows pgx.Rows) (api.ExportRecord, error) {
	var d api.ExportDependency
	var metadata string
	if err := rows.Scan(&d.IssueID, &d.DependsOn, &d.Type, &metadata, &d.ThreadID, &d.CreatedBy, &d.CreatedAt); err != nil {
		return api.ExportRecord{}, err
	}
	if metadata != "" {
		d.Metadata = json.RawMessage(metadata)
	}
	return api.ExportRecord{Type: api.RecordDependency, Dependency: &d}, nil
}

func scanExportComment(rows pgx.Rows) (api.ExportRecord, error) {
	var c api.Comment
	if err := rows.Scan(&c.ID, &c.IssueID, &c.Author, &c.Text, &c.CreatedAt); err != nil {
		return api.ExportRecord{}, err
	}
	return api.ExportRecord{Type: api.RecordComment, Comment: &c}, nil
}

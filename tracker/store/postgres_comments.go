package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// validateComment refuses a comment with no author or no text, whatever the
// issue; callers check it before writing anything.
func validateComment(author, text string) error {
	if strings.TrimSpace(author) == "" {
		return api.Invalid("a comment needs an author")
	}
	if strings.TrimSpace(text) == "" {
		return api.Invalid("a comment cannot be empty")
	}
	return nil
}

// AddComment appends a comment by author to an issue and counts it as
// activity on the issue (updated_at). Author and text are required.
func (p *Postgres) AddComment(ctx context.Context, id, author, text string) error {
	if err := validateComment(author, text); err != nil {
		return err
	}
	return p.inTx(ctx, func(tx pgx.Tx) error { return addCommentInTx(ctx, tx, id, author, text) })
}

// addCommentInTx appends the comment and records the activity in tx; the
// caller has checked the author and text with validateComment.
func addCommentInTx(ctx context.Context, tx pgx.Tx, id, author, text string) error {
	if _, err := lockIssue(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO tracker.comments (issue_id, author, text) VALUES ($1, $2, $3)", id, author, text); err != nil {
		return fmt.Errorf("store: add comment: %w", err)
	}
	if _, err := tx.Exec(ctx, "UPDATE tracker.issues SET updated_at = now() WHERE id = $1", id); err != nil {
		return fmt.Errorf("store: touch %s: %w", id, err)
	}
	// The comment text rides the event: without it an events feed cannot
	// replay the comment onto a Dolt rig that flipped back (gt tracker replay).
	return appendEvent(ctx, tx, "commented", id, author, nil, &text, nil)
}

// Comments returns an issue's comments, oldest first.
func (p *Postgres) Comments(ctx context.Context, id string) ([]api.Comment, error) {
	var exists bool
	if err := p.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tracker.issues WHERE id = $1)", id).Scan(&exists); err != nil {
		return nil, fmt.Errorf("store: comments: %w", err)
	}
	if !exists {
		return nil, api.NotFound(id)
	}
	rows, err := p.pool.Query(ctx, "SELECT id::text, issue_id, author, text, created_at FROM tracker.comments WHERE issue_id = $1 ORDER BY created_at, id", id)
	if err != nil {
		return nil, fmt.Errorf("store: comments: %w", err)
	}
	defer rows.Close()
	out := []api.Comment{}
	for rows.Next() {
		var c api.Comment
		if err := rows.Scan(&c.ID, &c.IssueID, &c.Author, &c.Text, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: comments: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

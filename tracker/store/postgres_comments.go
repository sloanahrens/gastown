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

// CommentCounts returns how many comments each of ids has, keyed by id, in one
// read. An id the store does not hold is api.ErrNotFound; an issue with no
// comments counts 0. Empty ids are ignored.
func (p *Postgres) CommentCounts(ctx context.Context, ids []string) (map[string]int, error) {
	out := map[string]int{}
	ids = uniqueStrings(ids)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT i.id, count(c.id)
		FROM tracker.issues i
		LEFT JOIN tracker.comments c ON c.issue_id = i.id
		WHERE i.id = ANY($1)
		GROUP BY i.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: comment counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: comment counts: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: comment counts: %w", err)
	}
	// A named id absent from the rows is one the store does not hold: refused,
	// never answered as a count of 0.
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, api.NotFound(id)
		}
	}
	return out, nil
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

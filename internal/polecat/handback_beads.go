package polecat

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
)

// handBackStore is the slice of beads.Client the production BeadWriter uses.
// *beads.Beads satisfies it; a test passes a fake.
type handBackStore interface {
	Update(id string, opts beads.UpdateOptions) error
	AddCommentAs(id, author, comment string) error
}

// beadsWriter is the production BeadWriter: the same `bd update` and
// `bd comments add` calls patrol_scan and gt done make to release a bead.
type beadsWriter struct {
	store handBackStore
	actor string
}

// NewBeadWriter returns the BeadWriter that writes through store, recording
// comments as written by actor. It reaches no branch or worktree: the store
// is bd.
func NewBeadWriter(store handBackStore, actor string) BeadWriter {
	return &beadsWriter{store: store, actor: actor}
}

func (w *beadsWriter) SetStatus(ctx context.Context, id, status string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return w.store.Update(id, beads.UpdateOptions{Status: &status})
}

// ClearAssignee passes an empty assignee, which bd update reads as "nobody"
// (--assignee=).
func (w *beadsWriter) ClearAssignee(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	none := ""
	return w.store.Update(id, beads.UpdateOptions{Assignee: &none})
}

func (w *beadsWriter) AddLabel(ctx context.Context, id, label string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return w.store.Update(id, beads.UpdateOptions{AddLabels: []string{label}})
}

func (w *beadsWriter) Comment(ctx context.Context, id, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.store.AddCommentAs(id, w.actor, text); err != nil {
		return fmt.Errorf("comment on %s: %w", id, err)
	}
	return nil
}

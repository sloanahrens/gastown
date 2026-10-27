package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// builtinStatuses are the statuses every database has; others come from
// tracker.custom_statuses.
var builtinStatuses = []string{"open", "in_progress", "closed", "deferred", "hooked", "pinned"}

func isBuiltinStatus(s string) bool { return slices.Contains(builtinStatuses, s) }

// appendNote adds line to notes on a new line, leaving notes alone when line
// is empty and not doubling a trailing newline.
func appendNote(notes, line string) string {
	if line == "" {
		return notes
	}
	if notes == "" {
		return line
	}
	return strings.TrimRight(notes, "\n") + "\n" + line
}

// current is the state of an issue a write reads under its row lock.
type current struct {
	title, description, acceptance, notes, status, assignee string
	priority                                                int
	ephemeral                                               bool
	startedAt                                               *time.Time
}

func lockIssue(ctx context.Context, tx pgx.Tx, id string) (current, error) {
	var c current
	err := tx.QueryRow(ctx, `SELECT title, description, acceptance_criteria, notes, status, assignee, priority, ephemeral, started_at
FROM tracker.issues WHERE id = $1 FOR UPDATE`, id).Scan(&c.title, &c.description, &c.acceptance, &c.notes, &c.status, &c.assignee, &c.priority, &c.ephemeral, &c.startedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, api.NotFound(id)
	}
	if err != nil {
		return c, fmt.Errorf("store: lock %s: %w", id, err)
	}
	return c, nil
}

func validateStatus(ctx context.Context, tx pgx.Tx, status string) error {
	if isBuiltinStatus(status) {
		return nil
	}
	var ok bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tracker.custom_statuses WHERE name = $1)", status).Scan(&ok); err != nil {
		return fmt.Errorf("store: check status: %w", err)
	}
	if !ok {
		return api.Invalid("unknown status %q", status)
	}
	return nil
}

// validatePatch refuses a patch the store cannot apply, whatever the issue;
// callers check it before writing anything.
func validatePatch(patch api.UpdatePatch) error {
	if patch.Title != nil && strings.TrimSpace(*patch.Title) == "" {
		return api.Invalid("title cannot be empty")
	}
	return nil
}

// UpdateIssue applies patch to one issue in a transaction while g holds. The
// guards compare the assignee and status under the row lock, so two callers
// racing to claim an issue cannot both win. Without a guard, overwriting
// another actor's live in_progress claim is refused unless patch.Force.
func (p *Postgres) UpdateIssue(ctx context.Context, id string, patch api.UpdatePatch, g api.Guard) error {
	if err := validatePatch(patch); err != nil {
		return err
	}
	if patch.Actor == "" {
		patch.Actor = api.ActorFrom(ctx)
	}
	return p.inTx(ctx, func(tx pgx.Tx) error { return updateInTx(ctx, tx, id, patch, g) })
}

func updateInTx(ctx context.Context, tx pgx.Tx, id string, patch api.UpdatePatch, g api.Guard) error {
	cur, err := lockIssue(ctx, tx, id)
	if err != nil {
		return err
	}
	if g.IfAssignee != nil && cur.assignee != *g.IfAssignee {
		return api.GuardFailed(fmt.Sprintf("%s is assigned to %q, not %q", id, cur.assignee, *g.IfAssignee))
	}
	if g.IfStatus != nil && cur.status != *g.IfStatus {
		return api.GuardFailed(fmt.Sprintf("%s has status %q, not %q", id, cur.status, *g.IfStatus))
	}
	if patch.Assignee != nil && *patch.Assignee != cur.assignee && g.IfAssignee == nil && !patch.Force &&
		cur.status == "in_progress" && cur.assignee != "" && patch.Actor != cur.assignee {
		return api.GuardFailed(fmt.Sprintf("%s is held in_progress by %q; pass a guard, or force an abandoned claim", id, cur.assignee))
	}

	var sets []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	type ev struct {
		op       string
		old, new *string
		comment  *string
	}
	var events []ev
	str := func(s string) *string { return &s }
	closing := false

	if patch.Title != nil && *patch.Title != cur.title {
		sets = append(sets, "title = "+arg(*patch.Title))
		events = append(events, ev{op: "updated", old: str(cur.title), new: patch.Title, comment: str("title")})
	}
	if patch.Priority != nil && *patch.Priority != cur.priority {
		sets = append(sets, "priority = "+arg(*patch.Priority))
		events = append(events, ev{op: "updated", old: str(strconv.Itoa(cur.priority)), new: str(strconv.Itoa(*patch.Priority)), comment: str("priority")})
	}
	if patch.Description != nil && *patch.Description != cur.description {
		sets = append(sets, "description = "+arg(*patch.Description))
		events = append(events, ev{op: "updated", comment: str("description")})
	}
	if patch.Acceptance != nil && *patch.Acceptance != cur.acceptance {
		sets = append(sets, "acceptance_criteria = "+arg(*patch.Acceptance))
		events = append(events, ev{op: "updated", comment: str("acceptance_criteria")})
	}
	if patch.Assignee != nil && *patch.Assignee != cur.assignee {
		sets = append(sets, "assignee = "+arg(*patch.Assignee))
		events = append(events, ev{op: "updated", old: str(cur.assignee), new: patch.Assignee, comment: str("assignee")})
	}
	if patch.AppendNotes != "" {
		sets = append(sets, "notes = "+arg(appendNote(cur.notes, patch.AppendNotes)))
		events = append(events, ev{op: "updated", comment: str("notes")})
	}
	if patch.DeferUntil != nil {
		if patch.DeferUntil.IsZero() {
			sets = append(sets, "defer_until = NULL")
		} else {
			sets = append(sets, "defer_until = "+arg(*patch.DeferUntil))
		}
		events = append(events, ev{op: "updated", comment: str("defer_until")})
	}
	if patch.Persistent && cur.ephemeral {
		sets = append(sets, "ephemeral = false")
		events = append(events, ev{op: "updated", comment: str("promoted to persistent")})
	}
	if patch.Status != nil && *patch.Status != cur.status {
		ns := *patch.Status
		if err := validateStatus(ctx, tx, ns); err != nil {
			return err
		}
		switch {
		case ns == "closed":
			closing = true
		case cur.status == "closed":
			sets = append(sets, "status = "+arg(ns), "closed_at = NULL", "close_reason = ''")
			events = append(events, ev{op: "reopened", old: str(cur.status), new: str(ns)})
		default:
			sets = append(sets, "status = "+arg(ns))
			events = append(events, ev{op: "status_changed", old: str(cur.status), new: str(ns)})
		}
		if ns == "in_progress" && cur.startedAt == nil {
			sets = append(sets, "started_at = now()")
		}
	}

	labelEvents, err := applyLabels(ctx, tx, id, patch)
	if err != nil {
		return err
	}

	if len(sets) > 0 {
		sets = append(sets, "updated_at = now()")
		args = append(args, id)
		if _, err := tx.Exec(ctx, "UPDATE tracker.issues SET "+strings.Join(sets, ", ")+" WHERE id = $"+strconv.Itoa(len(args)), args...); err != nil {
			return fmt.Errorf("store: update %s: %w", id, err)
		}
	} else if len(labelEvents) > 0 {
		if _, err := tx.Exec(ctx, "UPDATE tracker.issues SET updated_at = now() WHERE id = $1", id); err != nil {
			return fmt.Errorf("store: update %s: %w", id, err)
		}
	}
	for _, e := range events {
		if err := appendEvent(ctx, tx, e.op, id, patch.Actor, e.old, e.new, e.comment); err != nil {
			return err
		}
	}
	for _, e := range labelEvents {
		if err := appendEvent(ctx, tx, e.op, id, patch.Actor, nil, &e.label, nil); err != nil {
			return err
		}
	}
	// Closing is checked last but shares this transaction: a refused close
	// (open children or blockers) rolls back every change made above.
	if closing {
		return closeRow(ctx, tx, id, "", patch.Force, patch.Actor, []string{id}, false)
	}
	return nil
}

type labelEvent struct{ op, label string }

// applyLabels applies SetLabels, then AddLabels, then RemoveLabels, and
// returns one event per label actually added or removed.
func applyLabels(ctx context.Context, tx pgx.Tx, id string, patch api.UpdatePatch) ([]labelEvent, error) {
	if patch.SetLabels == nil && len(patch.AddLabels) == 0 && len(patch.RemoveLabels) == 0 {
		return nil, nil
	}
	have, err := stringsFrom(ctx, tx, "SELECT label FROM tracker.labels WHERE issue_id = $1", id)
	if err != nil {
		return nil, err
	}
	target := slices.Clone(have)
	if patch.SetLabels != nil {
		target = uniqueStrings(patch.SetLabels)
	}
	target = uniqueStrings(append(target, patch.AddLabels...))
	target = slices.DeleteFunc(target, func(l string) bool { return slices.Contains(patch.RemoveLabels, l) })

	var events []labelEvent
	for _, l := range uniqueStrings(have) {
		if !slices.Contains(target, l) {
			if _, err := tx.Exec(ctx, "DELETE FROM tracker.labels WHERE issue_id = $1 AND label = $2", id, l); err != nil {
				return nil, fmt.Errorf("store: remove label: %w", err)
			}
			events = append(events, labelEvent{"label_removed", l})
		}
	}
	for _, l := range target {
		if !slices.Contains(have, l) {
			if _, err := tx.Exec(ctx, "INSERT INTO tracker.labels (issue_id, label) VALUES ($1, $2)", id, l); err != nil {
				return nil, fmt.Errorf("store: add label: %w", err)
			}
			events = append(events, labelEvent{"label_added", l})
		}
	}
	return events, nil
}

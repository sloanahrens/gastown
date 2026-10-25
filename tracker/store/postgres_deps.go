package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

var depTypeRE = regexp.MustCompile(`^[a-z][a-z-]{0,30}$`)

// validateDepType accepts a lower-case token; the set of types in use is open
// (blocks, parent-child, tracks, relates-to, discovered-from, ...).
func validateDepType(t string) error {
	if !depTypeRE.MatchString(t) {
		return api.Invalid("dependency type %q must be lower-case letters and dashes", t)
	}
	return nil
}

// isCycleChecked reports whether links of this type must stay acyclic: a
// cycle among blocking links would leave every member permanently blocked.
func isCycleChecked(t string) bool { return t == "blocks" || t == "parent-child" }

// parseExternalRef splits external:<rig>:<id>; id may itself contain colons.
func parseExternalRef(ref string) (rig, id string, ok bool) {
	rest, found := strings.CutPrefix(ref, "external:")
	if !found {
		return "", "", false
	}
	rig, id, found = strings.Cut(rest, ":")
	if !found || rig == "" || id == "" {
		return "", "", false
	}
	return rig, id, true
}

// AddDependency records that issue depends on dependsOn with relation
// depType. Both must exist unless dependsOn is external:<rig>:<id>. Adding the
// same link again is a no-op; the same pair with another type is a conflict;
// a blocking link that would close a cycle is a conflict. A parent-child link
// also sets the child's parent.
func (p *Postgres) AddDependency(ctx context.Context, issue, dependsOn, depType string) error {
	if err := validateDepType(depType); err != nil {
		return err
	}
	if issue == dependsOn {
		return api.Invalid("%s cannot depend on itself", issue)
	}
	_, _, external := parseExternalRef(dependsOn)
	if strings.HasPrefix(dependsOn, "external:") && !external {
		return api.Invalid("external reference %q must be external:<rig>:<id>", dependsOn)
	}
	actor := api.ActorFrom(ctx)
	return p.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockIssue(ctx, tx, issue); err != nil {
			return err
		}
		if !external {
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tracker.issues WHERE id = $1)", dependsOn).Scan(&exists); err != nil {
				return fmt.Errorf("store: check dependency target: %w", err)
			}
			if !exists {
				return api.NotFound(dependsOn)
			}
		}
		var have string
		err := tx.QueryRow(ctx, "SELECT type FROM tracker.dependencies WHERE issue_id = $1 AND depends_on = $2", issue, dependsOn).Scan(&have)
		switch {
		case err == nil && have == depType:
			return nil
		case err == nil:
			return api.Conflict("%s already depends on %s as %q", issue, dependsOn, have)
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("store: check dependency: %w", err)
		}
		if isCycleChecked(depType) && !external {
			var cycle bool
			if err := tx.QueryRow(ctx, `WITH RECURSIVE walk(id) AS (
  SELECT $2::text
  UNION
  SELECT d.depends_on FROM tracker.dependencies d JOIN walk w ON d.issue_id = w.id WHERE d.type IN ('blocks', 'parent-child')
) SELECT EXISTS (SELECT 1 FROM walk WHERE id = $1)`, issue, dependsOn).Scan(&cycle); err != nil {
				return fmt.Errorf("store: check cycle: %w", err)
			}
			if cycle {
				return api.Conflict("%s -> %s would make a cycle of blocking links", issue, dependsOn)
			}
		}
		if depType == "parent-child" {
			var parent string
			if err := tx.QueryRow(ctx, "SELECT coalesce(parent_id, '') FROM tracker.issues WHERE id = $1", issue).Scan(&parent); err != nil {
				return fmt.Errorf("store: read parent: %w", err)
			}
			if parent != "" && parent != dependsOn {
				return api.Conflict("%s already has parent %s", issue, parent)
			}
			if _, err := tx.Exec(ctx, "UPDATE tracker.issues SET parent_id = $2 WHERE id = $1", issue, dependsOn); err != nil {
				return fmt.Errorf("store: set parent: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.dependencies (issue_id, depends_on, type, created_by) VALUES ($1, $2, $3, $4)", issue, dependsOn, depType, actor); err != nil {
			return fmt.Errorf("store: add dependency: %w", err)
		}
		if _, err := tx.Exec(ctx, "UPDATE tracker.issues SET updated_at = now() WHERE id = $1", issue); err != nil {
			return fmt.Errorf("store: touch %s: %w", issue, err)
		}
		return appendEvent(ctx, tx, "dependency_added", issue, actor, nil, &dependsOn, &depType)
	})
}

// RemoveDependency removes the link from issue to dependsOn, whatever its
// type, and clears the child's parent if it was a parent-child link. A link
// that does not exist is not found.
func (p *Postgres) RemoveDependency(ctx context.Context, issue, dependsOn string) error {
	actor := api.ActorFrom(ctx)
	return p.inTx(ctx, func(tx pgx.Tx) error {
		var typ string
		err := tx.QueryRow(ctx, "DELETE FROM tracker.dependencies WHERE issue_id = $1 AND depends_on = $2 RETURNING type", issue, dependsOn).Scan(&typ)
		if errors.Is(err, pgx.ErrNoRows) {
			return api.NotFound(fmt.Sprintf("dependency %s -> %s", issue, dependsOn))
		}
		if err != nil {
			return fmt.Errorf("store: remove dependency: %w", err)
		}
		if typ == "parent-child" {
			if _, err := tx.Exec(ctx, "UPDATE tracker.issues SET parent_id = NULL WHERE id = $1 AND parent_id = $2", issue, dependsOn); err != nil {
				return fmt.Errorf("store: clear parent: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE tracker.issues SET updated_at = now() WHERE id = $1", issue); err != nil {
			return fmt.Errorf("store: touch %s: %w", issue, err)
		}
		return appendEvent(ctx, tx, "dependency_removed", issue, actor, &dependsOn, nil, &typ)
	})
}

// Dependencies returns what id depends on with relation depType ("" for any),
// each with its relation. External targets have no row here and are left out.
func (p *Postgres) Dependencies(ctx context.Context, id, depType string) ([]api.IssueDep, error) {
	var exists bool
	if err := p.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tracker.issues WHERE id = $1)", id).Scan(&exists); err != nil {
		return nil, fmt.Errorf("store: dependencies: %w", err)
	}
	if !exists {
		return nil, api.NotFound(id)
	}
	rows, err := p.pool.Query(ctx, `SELECT t.id, t.title, t.status, t.priority, t.issue_type, d.type, t.close_reason
FROM tracker.dependencies d JOIN tracker.issues t ON t.id = d.depends_on
WHERE d.issue_id = $1 AND ($2 = '' OR d.type = $2) ORDER BY d.type, t.id`, id, depType)
	if err != nil {
		return nil, fmt.Errorf("store: dependencies: %w", err)
	}
	defer rows.Close()
	out := []api.IssueDep{}
	for rows.Next() {
		var d api.IssueDep
		if err := rows.Scan(&d.ID, &d.Title, &d.Status, &d.Priority, &d.Type, &d.DependencyType, &d.CloseReason); err != nil {
			return nil, fmt.Errorf("store: dependencies: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

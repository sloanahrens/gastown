package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// idAttempts is how many fresh generated ids CreateIssue tries before it
// gives up on collisions.
const idAttempts = 5

// idSource is where generated ids get their randomness; tests replace it.
var idSource io.Reader = rand.Reader

// CreateIssue creates an issue or wisp. The rig comes from the id, the Rig
// field or the parent's id; an unknown rig is invalid and a taken id is a
// conflict. A generated id that collides is retried with a fresh one.
func (p *Postgres) CreateIssue(ctx context.Context, params api.CreateParams) (api.Issue, error) {
	rig, err := validateCreate(params)
	if err != nil {
		return api.Issue{}, err
	}
	attempts := 1
	if params.ID == "" && params.Parent == "" {
		attempts = idAttempts // only a random id can collide by chance
	}
	var made api.Issue
	for i := 0; i < attempts; i++ {
		err = p.inTx(ctx, func(tx pgx.Tx) error {
			var err error
			made, err = createInTx(ctx, tx, params, rig)
			return err
		})
		if err == nil {
			return made, nil
		}
		if i+1 < attempts && isIDCollision(err) {
			continue
		}
		return api.Issue{}, err
	}
	return api.Issue{}, err
}

// validateCreate refuses a create the store cannot apply, whatever the issue,
// and returns the rig it names. Batch checks it for each create, so a batched
// create refuses exactly what the single create refuses.
func validateCreate(params api.CreateParams) (string, error) {
	if strings.TrimSpace(params.Title) == "" {
		return "", api.Invalid("title is required")
	}
	rig, err := rigForCreate(params)
	if err != nil {
		return "", err
	}
	if params.EventPayload != "" && !json.Valid([]byte(params.EventPayload)) {
		return "", api.Invalid("event_payload is not valid JSON")
	}
	return rig, nil
}

// isIDCollision reports whether err is the api conflict createInTx returns
// for a taken id.
func isIDCollision(err error) bool {
	ae, ok := err.(*api.Error)
	return ok && ae.Kind == api.KindConflict
}

// rigForCreate decides the rig and checks it against the id and the parent.
func rigForCreate(p api.CreateParams) (string, error) {
	var fromID, fromParent string
	if p.ID != "" {
		if fromID = rigOfID(p.ID); fromID == "" {
			return "", api.Invalid("id %q has no rig prefix", p.ID)
		}
	}
	if p.Parent != "" {
		if fromParent = rigOfID(p.Parent); fromParent == "" {
			return "", api.Invalid("parent %q has no rig prefix", p.Parent)
		}
	}
	rig := p.Rig
	for _, other := range []string{fromID, fromParent} {
		switch {
		case other == "":
		case rig == "":
			rig = other
		case rig != other:
			return "", api.Invalid("rig %q does not match id or parent prefix %q", rig, other)
		}
	}
	if rig == "" {
		return "", api.Invalid("a rig, an id or a parent is required")
	}
	return rig, nil
}

func createInTx(ctx context.Context, tx pgx.Tx, p api.CreateParams, rig string) (api.Issue, error) {
	id := p.ID
	if p.Parent != "" {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tracker.issues WHERE id = $1)", p.Parent).Scan(&exists); err != nil {
			return api.Issue{}, fmt.Errorf("store: check parent: %w", err)
		}
		if !exists {
			return api.Issue{}, api.NotFound("parent " + p.Parent)
		}
	}
	switch {
	case id != "":
	case p.Parent != "":
		var err error
		if id, err = nextChildID(ctx, tx, p.Parent); err != nil {
			return api.Issue{}, err
		}
	case p.Ephemeral:
		var err error
		if id, err = newWispID(idSource, rig); err != nil {
			return api.Issue{}, err
		}
	default:
		var err error
		if id, err = newID(idSource, rig); err != nil {
			return api.Issue{}, err
		}
	}
	typ := p.Type
	if typ == "" {
		typ = "task"
	}
	var payload any
	if p.EventPayload != "" {
		payload = p.EventPayload
	}
	var parentID any
	if p.Parent != "" {
		parentID = p.Parent
	}
	_, err := tx.Exec(ctx, `INSERT INTO tracker.issues
(id, rig, issue_type, status, title, description, acceptance_criteria, priority, assignee, created_by, ephemeral, event_kind, payload, parent_id)
VALUES ($1, $2, $3, 'open', $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13)`,
		id, rig, typ, p.Title, p.Description, p.Acceptance, p.Priority, p.Assignee, p.Actor, p.Ephemeral, p.EventKind, payload, parentID)
	if err != nil {
		code, constraint := pgCode(err)
		switch {
		case code == sqlUniqueViolation && constraint == "issues_pkey":
			return api.Issue{}, api.Conflict("issue %s already exists", id)
		case code == sqlForeignKeyViolation && constraint == "issues_rig_fkey":
			return api.Issue{}, api.Invalid("unknown rig %q", rig)
		}
		return api.Issue{}, fmt.Errorf("store: create %s: %w", id, err)
	}
	for _, label := range uniqueStrings(p.Labels) {
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.labels (issue_id, label) VALUES ($1, $2)", id, label); err != nil {
			return api.Issue{}, fmt.Errorf("store: create %s: label %q: %w", id, label, err)
		}
	}
	if p.Parent != "" {
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.dependencies (issue_id, depends_on, type, created_by) VALUES ($1, $2, 'parent-child', $3)", id, p.Parent, p.Actor); err != nil {
			return api.Issue{}, fmt.Errorf("store: create %s: link to parent: %w", id, err)
		}
	}
	title := p.Title
	if err := appendEvent(ctx, tx, "created", id, p.Actor, nil, &title, nil); err != nil {
		return api.Issue{}, err
	}
	for _, label := range uniqueStrings(p.Labels) {
		if err := appendEvent(ctx, tx, "label_added", id, p.Actor, nil, &label, nil); err != nil {
			return api.Issue{}, err
		}
	}
	is, err := scanIssue(tx.QueryRow(ctx, "SELECT "+issueColumns+" FROM tracker.issues WHERE id = $1", id))
	if err != nil {
		return api.Issue{}, fmt.Errorf("store: read back %s: %w", id, err)
	}
	is.Labels = uniqueStrings(p.Labels)
	if len(is.Labels) == 0 {
		is.Labels = nil
	}
	return is, nil
}

// GetIssues returns the issues that exist among ids, with their labels.
func (p *Postgres) GetIssues(ctx context.Context, ids []string) (map[string]api.Issue, error) {
	out := map[string]api.Issue{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, "SELECT "+issueColumns+" FROM tracker.issues WHERE id = ANY($1)", ids)
	if err != nil {
		return nil, fmt.Errorf("store: get issues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("store: get issues: %w", err)
		}
		out[is.ID] = is
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: get issues: %w", err)
	}
	found := make([]string, 0, len(out))
	for id := range out {
		found = append(found, id)
	}
	labels, err := loadLabels(ctx, p.pool, found)
	if err != nil {
		return nil, err
	}
	for id, ls := range labels {
		is := out[id]
		is.Labels = ls
		out[id] = is
	}
	if err := fillDependencies(ctx, p.pool, found, out); err != nil {
		return nil, err
	}
	if err := fillDependents(ctx, p.pool, found, out); err != nil {
		return nil, err
	}
	return out, nil
}

// fillDependencies sets Dependencies and BlockedBy on the issues in out named
// by ids: what each depends on, with the target's title and status, and which
// of the blocking targets are still open. Targets that are not issues here
// (external references) are left out.
func fillDependencies(ctx context.Context, q querier, ids []string, out map[string]api.Issue) error {
	rows, err := q.Query(ctx, `SELECT d.issue_id, t.id, t.title, t.status, t.priority, t.issue_type, d.type, t.close_reason,
  (d.type IN `+blocking+` AND `+unresolved+`) AS blocks_now
FROM tracker.dependencies d JOIN tracker.issues t ON t.id = d.depends_on
WHERE d.issue_id = ANY($1) ORDER BY d.issue_id, d.created_at, t.id`, ids)
	if err != nil {
		return fmt.Errorf("store: get dependencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var owner string
		var dep api.IssueDep
		var priority int16
		var blocksNow bool
		if err := rows.Scan(&owner, &dep.ID, &dep.Title, &dep.Status, &priority, &dep.Type, &dep.DependencyType, &dep.CloseReason, &blocksNow); err != nil {
			return fmt.Errorf("store: get dependencies: %w", err)
		}
		dep.Priority = int(priority)
		is, ok := out[owner]
		if !ok {
			continue // the owner went away between the two reads
		}
		is.Dependencies = append(is.Dependencies, dep)
		if blocksNow {
			is.BlockedBy = append(is.BlockedBy, dep.ID)
		}
		out[owner] = is
	}
	return rows.Err()
}

// fillDependents sets Dependents on the issues in out named by ids: the issues
// that depend on each, with the relation. A link names its target plainly, or
// as external:<rig>:<id> when the id is the reference's last colon-separated
// part; that part must match exactly (gt-12 does not name gt-2). A dependent
// holding both kinds of link to one id is listed once, at the plain link's
// relation. Links from other rigs' external references to ids not in out are
// not issues here and do not appear.
func fillDependents(ctx context.Context, q querier, ids []string, out map[string]api.Issue) error {
	rows, err := q.Query(ctx, `WITH links AS (
  SELECT CASE WHEN d.depends_on = ANY($1) THEN d.depends_on ELSE regexp_replace(d.depends_on, '^.*:', '') END AS target,
    d.issue_id, d.type, d.created_at, d.depends_on = ANY($1) AS plain
  FROM tracker.dependencies d
  WHERE d.depends_on = ANY($1) OR (d.depends_on LIKE 'external:%:%' AND regexp_replace(d.depends_on, '^.*:', '') = ANY($1))
), one AS (
  SELECT DISTINCT ON (target, issue_id) target, issue_id, type, created_at
  FROM links ORDER BY target, issue_id, plain DESC, created_at
)
SELECT o.target, i.id, i.title, i.status, i.priority, i.issue_type, o.type, i.close_reason
FROM one o JOIN tracker.issues i ON i.id = o.issue_id
ORDER BY o.target, o.created_at, i.id`, ids)
	if err != nil {
		return fmt.Errorf("store: get dependents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var target string
		var dep api.IssueDep
		var priority int16
		if err := rows.Scan(&target, &dep.ID, &dep.Title, &dep.Status, &priority, &dep.Type, &dep.DependencyType, &dep.CloseReason); err != nil {
			return fmt.Errorf("store: get dependents: %w", err)
		}
		dep.Priority = int(priority)
		is, ok := out[target]
		if !ok {
			continue
		}
		is.Dependents = append(is.Dependents, dep)
		out[target] = is
	}
	return rows.Err()
}

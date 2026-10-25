package migrate

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/api"
)

// Options tunes Import.
type Options struct {
	// RigNames maps a rig prefix to its display name; a prefix not listed is
	// named after itself.
	RigNames map[string]string
}

// Report says what Import wrote and what it left out, by table. A skipped row
// is a row Dolt had that the tracker cannot hold (an orphan, a duplicate); the
// import never drops one without counting it here.
type Report struct {
	Written map[string]int
	Skipped map[string]int
}

func newReport() Report { return Report{Written: map[string]int{}, Skipped: map[string]int{}} }

// Import writes ds into the tracker in one transaction: all of it or none.
// It refuses to run when any of ds's issue ids already exist, so a second run
// cannot double the data; to redo an import, start from an empty database.
func Import(ctx context.Context, pool *pgxpool.Pool, ds *Dataset, opt Options) (Report, error) {
	rep := newReport()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return rep, fmt.Errorf("migrate: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := importAll(ctx, tx, ds, opt, &rep); err != nil {
		return Report{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Report{}, fmt.Errorf("migrate: commit: %w", err)
	}
	return rep, nil
}

func importAll(ctx context.Context, tx pgx.Tx, ds *Dataset, opt Options, rep *Report) error {
	ids := make([]string, len(ds.Issues))
	have := make(map[string]bool, len(ds.Issues))
	for i, is := range ds.Issues {
		ids[i] = is.ID
		have[is.ID] = true
	}
	var existing int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM tracker.issues WHERE id = ANY($1)", ids).Scan(&existing); err != nil {
		return fmt.Errorf("migrate: check existing: %w", err)
	}
	if existing > 0 {
		return api.Conflict("%d of the %d issues to import already exist in the tracker; import into an empty database", existing, len(ids))
	}
	for _, rig := range rigsOf(ids) {
		name := rig
		if n := opt.RigNames[rig]; n != "" {
			name = n
		}
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ($1, $2) ON CONFLICT (prefix) DO NOTHING", rig, name); err != nil {
			return fmt.Errorf("migrate: rig %s: %w", rig, err)
		}
	}
	if err := copyIssues(ctx, tx, ds, rep); err != nil {
		return err
	}
	if err := copyDeps(ctx, tx, ds, have, rep); err != nil {
		return err
	}
	if err := copyLabels(ctx, tx, ds, have, rep); err != nil {
		return err
	}
	if err := copyComments(ctx, tx, ds, have, rep); err != nil {
		return err
	}
	if err := copyEvents(ctx, tx, ds, rep); err != nil {
		return err
	}
	return writeSettings(ctx, tx, ds, rep)
}

func copyIssues(ctx context.Context, tx pgx.Tx, ds *Dataset, rep *Report) error {
	parents := parentsOf(ds.Deps)
	rows := make([][]any, 0, len(ds.Issues))
	for _, is := range ds.Issues {
		rig, _, _ := strings.Cut(is.ID, "-")
		var payload any
		if is.Payload != "" {
			payload = []byte(is.Payload)
		}
		var parent any
		if p := parents[is.ID]; p != "" {
			parent = p
		}
		rows = append(rows, []any{is.ID, rig, is.Type, is.Status, is.Title, is.Description, is.Design, is.Acceptance, is.Notes,
			clampInt16(is.Priority), is.Assignee, is.Owner, is.CreatedBy, is.CreatedAt, is.UpdatedAt, is.StartedAt, is.ClosedAt, is.DeferUntil,
			is.CloseReason, is.Wisp, is.WispType, is.EventKind, payload, is.HookBead, is.AgentState, is.LastActivity, parent,
			legacyMetadata(is.Legacy)})
	}
	n, err := tx.CopyFrom(ctx, pgx.Identifier{"tracker", "issues"}, []string{"id", "rig", "issue_type", "status", "title", "description", "design",
		"acceptance_criteria", "notes", "priority", "assignee", "owner", "created_by", "created_at", "updated_at", "started_at", "closed_at",
		"defer_until", "close_reason", "ephemeral", "wisp_type", "event_kind", "payload", "hook_bead", "agent_state", "last_activity", "parent_id", "metadata"},
		pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("migrate: copy issues: %w", err)
	}
	rep.Written["issues"] = int(n)
	for _, is := range ds.Issues {
		if is.Wisp {
			rep.Written["issues.ephemeral"]++
		}
	}
	return nil
}

func copyDeps(ctx context.Context, tx pgx.Tx, ds *Dataset, have map[string]bool, rep *Report) error {
	seen := map[[2]string]bool{}
	var rows [][]any
	for _, d := range ds.Deps {
		key := [2]string{d.IssueID, d.DependsOn}
		switch {
		case !have[d.IssueID]:
			rep.Skipped["dependencies: issue not in the dataset"]++
		case d.DependsOn == "":
			rep.Skipped["dependencies: no target"]++
		case seen[key]:
			rep.Skipped["dependencies: duplicate link"]++
		default:
			seen[key] = true
			var meta any
			if d.Metadata != "" {
				meta = []byte(d.Metadata)
			}
			rows = append(rows, []any{d.IssueID, d.DependsOn, d.Type, meta, d.ThreadID, d.CreatedAt, d.CreatedBy})
		}
	}
	n, err := tx.CopyFrom(ctx, pgx.Identifier{"tracker", "dependencies"}, []string{"issue_id", "depends_on", "type", "metadata", "thread_id", "created_at", "created_by"}, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("migrate: copy dependencies: %w", err)
	}
	rep.Written["dependencies"] = int(n)
	return nil
}

func copyLabels(ctx context.Context, tx pgx.Tx, ds *Dataset, have map[string]bool, rep *Report) error {
	seen := map[[2]string]bool{}
	var rows [][]any
	for _, l := range ds.Labels {
		key := [2]string{l.IssueID, l.Label}
		switch {
		case !have[l.IssueID]:
			rep.Skipped["labels: issue not in the dataset"]++
		case l.Label == "":
			rep.Skipped["labels: empty label"]++
		case seen[key]:
			rep.Skipped["labels: duplicate"]++
		default:
			seen[key] = true
			rows = append(rows, []any{l.IssueID, l.Label})
		}
	}
	n, err := tx.CopyFrom(ctx, pgx.Identifier{"tracker", "labels"}, []string{"issue_id", "label"}, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("migrate: copy labels: %w", err)
	}
	rep.Written["labels"] = int(n)
	return nil
}

func copyComments(ctx context.Context, tx pgx.Tx, ds *Dataset, have map[string]bool, rep *Report) error {
	var rows [][]any
	for _, c := range ds.Comments {
		if !have[c.IssueID] {
			rep.Skipped["comments: issue not in the dataset"]++
			continue
		}
		rows = append(rows, []any{c.IssueID, c.Author, c.Text, c.CreatedAt})
	}
	n, err := tx.CopyFrom(ctx, pgx.Identifier{"tracker", "comments"}, []string{"issue_id", "author", "text", "created_at"}, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("migrate: copy comments: %w", err)
	}
	rep.Written["comments"] = int(n)
	return nil
}

// copyEvents writes the audit events in time order, so the feed's seq runs
// the way the history did. Events of issues no longer present are kept: they
// outlive their issue by design.
func copyEvents(ctx context.Context, tx pgx.Tx, ds *Dataset, rep *Report) error {
	evs := slices.Clone(ds.Events)
	slices.SortStableFunc(evs, func(a, b EventRow) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	rows := make([][]any, 0, len(evs))
	for _, e := range evs {
		rows = append(rows, []any{e.CreatedAt, e.Op, e.IssueID, e.Actor, e.OldValue, e.NewValue, e.Comment})
	}
	n, err := tx.CopyFrom(ctx, pgx.Identifier{"tracker", "events"}, []string{"ts", "op", "issue_id", "actor", "old_value", "new_value", "comment"}, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("migrate: copy events: %w", err)
	}
	rep.Written["events"] = int(n)
	return nil
}

func writeSettings(ctx context.Context, tx pgx.Tx, ds *Dataset, rep *Report) error {
	cfg, kv := splitConfig(ds.Config)
	for k, v := range cfg {
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.config (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value", k, v); err != nil {
			return fmt.Errorf("migrate: config %s: %w", k, err)
		}
	}
	for k, v := range kv {
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.kv (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value", k, v); err != nil {
			return fmt.Errorf("migrate: kv %s: %w", k, err)
		}
	}
	for _, t := range ds.CustomTypes {
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.custom_types (name) VALUES ($1) ON CONFLICT DO NOTHING", t); err != nil {
			return fmt.Errorf("migrate: custom type %s: %w", t, err)
		}
	}
	for _, s := range ds.CustomStatuses {
		cat := s.Category
		if cat == "" {
			cat = "unspecified"
		}
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.custom_statuses (name, category) VALUES ($1, $2) ON CONFLICT DO NOTHING", s.Name, cat); err != nil {
			return fmt.Errorf("migrate: custom status %s: %w", s.Name, err)
		}
	}
	for parent, last := range ds.ChildCounters {
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.child_counters (parent_id, last_child) VALUES ($1, $2) ON CONFLICT (parent_id) DO UPDATE SET last_child = GREATEST(tracker.child_counters.last_child, EXCLUDED.last_child)", parent, last); err != nil {
			return fmt.Errorf("migrate: child counter %s: %w", parent, err)
		}
	}
	rep.Written["config"], rep.Written["kv"] = len(cfg), len(kv)
	rep.Written["custom_types"], rep.Written["custom_statuses"] = len(ds.CustomTypes), len(ds.CustomStatuses)
	rep.Written["child_counters"] = len(ds.ChildCounters)
	return nil
}

// clampInt16 narrows a priority to smallint; real ones run -1 to 4.
func clampInt16(v int) int16 {
	switch {
	case v > math.MaxInt16:
		return math.MaxInt16
	case v < math.MinInt16:
		return math.MinInt16
	}
	return int16(v)
}

package migrate

import (
	"context"
	"errors"
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
	// Synthesized lists the parent-child links Import added for dotted ids
	// that Dolt holds none for, as "child -> parent".
	Synthesized []string
}

// VerifyError is returned by Import when the written data does not match the
// dataset. Nothing was committed.
type VerifyError struct{ Diff Diff }

func (e *VerifyError) Error() string {
	return fmt.Sprintf("migrate: the import does not match Dolt (%d difference(s)); nothing was committed", e.Diff.Total)
}

func newReport() Report { return Report{Written: map[string]int{}, Skipped: map[string]int{}} }

// Import writes ds into the tracker in one transaction: all of it or none.
// It refuses to run when any of ds's issue ids already exist, so a second run
// cannot double the data; to redo an import, start from an empty database.
func Import(ctx context.Context, pool *pgxpool.Pool, ds *Dataset, opt Options) (Report, error) {
	rep := newReport()
	if err := Validate(ds); err != nil {
		return rep, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return rep, fmt.Errorf("migrate: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := importAll(ctx, tx, ds, opt, &rep); err != nil {
		return Report{}, err
	}
	// Prove the copy before committing: a mismatch rolls everything back, so
	// the tracker never holds an import that does not match Dolt.
	diff, err := Verify(ctx, tx, ds)
	if err != nil {
		return Report{}, err
	}
	if !diff.Equal() {
		return Report{}, &VerifyError{Diff: diff}
	}
	if err := tx.Commit(ctx); err != nil {
		return Report{}, fmt.Errorf("migrate: commit: %w", err)
	}
	return rep, nil
}

func importAll(ctx context.Context, tx pgx.Tx, ds *Dataset, opt Options, rep *Report) error {
	ids := idsOf(ds)
	have := make(map[string]bool, len(ds.Issues))
	for _, id := range ids {
		have[id] = true
	}
	// Rig configuration is stored under the rig's name. A database whose rig
	// cannot be told would write its settings un-namespaced and collide with
	// the next rig's, so it is refused before anything is written.
	if cfg, _ := splitConfig(ds.Config); len(cfg) > 0 && primaryRig(ds) == "" {
		return api.Invalid("cannot tell which rig this database belongs to: it has no issue_prefix setting and its issues use %d rig prefixes", len(rigsOf(ids)))
	}
	// Writers of tracker.issues wait here until this transaction ends, so the
	// existence check below cannot be overtaken by a concurrent import.
	if _, err := tx.Exec(ctx, "LOCK TABLE tracker.issues IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("migrate: lock issues: %w", err)
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
	deps, synth := repairedDeps(ds)
	if err := copyIssues(ctx, tx, ds, deps, rep); err != nil {
		return err
	}
	if err := copyDeps(ctx, tx, deps, have, rep); err != nil {
		return err
	}
	for _, d := range synth {
		rep.Synthesized = append(rep.Synthesized, d.IssueID+" -> "+d.DependsOn)
	}
	if len(synth) > 0 {
		rep.Written["dependencies.synthesized"] = len(synth)
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

func copyIssues(ctx context.Context, tx pgx.Tx, ds *Dataset, deps []DepRow, rep *Report) error {
	parents := parentsOf(deps)
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

func copyDeps(ctx context.Context, tx pgx.Tx, deps []DepRow, have map[string]bool, rep *Report) error {
	seen := map[[2]string]bool{}
	var rows [][]any
	for _, d := range deps {
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
	// An event is kept only when its issue id belongs to one of the dataset's
	// own rigs. One about another rig's issue or about no issue could not be
	// told apart from that rig's own events afterwards, so it is counted as
	// skipped rather than written.
	rigs := rigsOf(idsOf(ds))
	evs := make([]EventRow, 0, len(ds.Events))
	for _, e := range ds.Events {
		if inRigs(e.IssueID, rigs) {
			evs = append(evs, e)
		} else {
			rep.Skipped["events: not about an issue of this database's rigs"]++
		}
	}
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
	rig := primaryRig(ds)
	for k, v := range cfg {
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.config (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value", configKey(rig, k), v); err != nil {
			return fmt.Errorf("migrate: config %s: %w", k, err)
		}
	}
	// Memories are town-wide. A key two rigs both hold with different values
	// is a conflict a person must settle; never overwritten silently. The
	// check-then-insert is safe because importAll holds a table lock on
	// tracker.issues that every other import also takes first.
	for k, v := range kv {
		var have string
		err := tx.QueryRow(ctx, "SELECT value FROM tracker.kv WHERE key = $1", k).Scan(&have)
		switch {
		case err == nil && have != v:
			return api.Conflict("memory %q already holds a different value in the tracker; settle which rig's value is right before importing", k)
		case err == nil:
			rep.Skipped["kv: same value already present"]++
			continue
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("migrate: kv %s: %w", k, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.kv (key, value) VALUES ($1, $2)", k, v); err != nil {
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

// Validate refuses a dataset that cannot be what a Dolt database holds: rows
// without the timestamps every row has. This is the check that notices a Dolt
// connection which did not parse datetimes (every time came back zero), which
// Verify cannot: it compares the tracker with the same zeros it was given.
func Validate(ds *Dataset) error {
	var bad []string
	count := func(what string, n int) {
		if n > 0 {
			bad = append(bad, fmt.Sprintf("%d %s", n, what))
		}
	}
	zeroIssues, zeroComments, zeroEvents := 0, 0, 0
	first := ""
	for _, is := range ds.Issues {
		if is.CreatedAt.IsZero() || is.UpdatedAt.IsZero() {
			if zeroIssues == 0 {
				first = is.ID
			}
			zeroIssues++
		}
	}
	for _, c := range ds.Comments {
		if c.CreatedAt.IsZero() {
			zeroComments++
		}
	}
	for _, e := range ds.Events {
		if e.CreatedAt.IsZero() {
			zeroEvents++
		}
	}
	count("issues without created_at or updated_at", zeroIssues)
	count("comments without created_at", zeroComments)
	count("events without created_at", zeroEvents)
	if len(bad) == 0 {
		return nil
	}
	return api.Invalid("the Dolt data has %s (first issue: %q); the connection probably did not parse datetimes, so nothing was imported", strings.Join(bad, ", "), first)
}

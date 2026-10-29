package migrate

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Diff is what Verify found. An empty Problems list means the tracker holds
// what the Dolt dataset held, row for row in every table checked. Problems
// lists the first few of each kind; Total counts every one.
type Diff struct {
	Problems []string
	Total    int
}

// Equal reports whether nothing differs.
func (d Diff) Equal() bool { return d.Total == 0 }

// Querier is what Verify reads through: a pool, a connection or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const problemLimit = 10

// problems keeps the first problemLimit of each kind and counts them all.
type problems struct {
	list  []string
	kinds map[string]int
	total int
}

func (p *problems) add(kind, format string, args ...any) {
	p.total++
	if p.kinds == nil {
		p.kinds = map[string]int{}
	}
	if p.kinds[kind]++; p.kinds[kind] <= problemLimit {
		p.list = append(p.list, fmt.Sprintf(format, args...))
	}
}

func (p *problems) diff() Diff {
	list := p.list
	for kind, n := range p.kinds {
		if n > problemLimit {
			list = append(list, fmt.Sprintf("%s: %d problems in all, the first %d are listed", kind, n, problemLimit))
		}
	}
	return Diff{Problems: list, Total: p.total}
}

func us(t time.Time) string { return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) }

func usPtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return us(*t)
}

// issueFingerprint is every issue column the importer writes, joined: the
// dataset side and the tracker side must produce the same string.
func issueFingerprint(f []string, labels []string) string {
	l := slices.Clone(labels)
	slices.Sort(l)
	return strings.Join(append(f, strings.Join(l, ",")), "\x1f")
}

func datasetFingerprint(is IssueRow, parent string, labels []string) string {
	return issueFingerprint([]string{is.ID, is.Type, is.Status, is.Title, is.Description, is.Design, is.Acceptance, is.Notes,
		fmt.Sprint(clampInt16(is.Priority)), is.Assignee, is.Owner, is.CreatedBy, us(is.CreatedAt), us(is.UpdatedAt),
		usPtr(is.StartedAt), usPtr(is.ClosedAt), usPtr(is.DeferUntil), is.CloseReason, fmt.Sprint(is.Wisp), is.WispType,
		is.EventKind, is.HookBead, is.AgentState, usPtr(is.LastActivity), parent}, labels)
}

// Verify compares ds with the tracker. It reads only. A query that fails is
// an error, never a count of zero.
func Verify(ctx context.Context, q Querier, ds *Dataset) (Diff, error) {
	var p problems
	// The rows Import wrote, synthesized links included: the same function
	// decides both, so a repaired import verifies.
	deps, _ := repairedDeps(ds)
	parents := parentsOf(deps)
	labelsBy := map[string][]string{}
	seenLabel := map[[2]string]bool{}
	for _, l := range ds.Labels {
		if k := [2]string{l.IssueID, l.Label}; !seenLabel[k] && l.Label != "" {
			seenLabel[k] = true
			labelsBy[l.IssueID] = append(labelsBy[l.IssueID], l.Label)
		}
	}
	// The tracker holds other rigs too: compare only the rigs in this dataset.
	rigs := rigsOf(idsOf(ds))
	want := make(map[string]string, len(ds.Issues))
	for _, is := range ds.Issues {
		want[is.ID] = datasetFingerprint(is, parents[is.ID], labelsBy[is.ID])
	}

	rows, err := q.Query(ctx, `SELECT i.id, i.issue_type, i.status, i.title, i.description, i.design, i.acceptance_criteria, i.notes,
i.priority, i.assignee, i.owner, i.created_by, i.created_at, i.updated_at, i.started_at, i.closed_at, i.defer_until,
i.close_reason, i.ephemeral, i.wisp_type, i.event_kind, i.hook_bead, i.agent_state, i.last_activity, coalesce(i.parent_id, ''),
coalesce((SELECT array_agg(l.label) FROM tracker.labels l WHERE l.issue_id = i.id), '{}')
FROM tracker.issues i WHERE i.rig = ANY($1)`, rigs)
	if err != nil {
		return Diff{}, fmt.Errorf("migrate: verify issues: %w", err)
	}
	got := map[string]string{}
	for rows.Next() {
		var id, typ, status, title, desc, design, acc, notes, assignee, owner, createdBy, closeReason, wispType, eventKind, hook, agentState, parent string
		var priority int16
		var ephemeral bool
		var created, updated time.Time
		var started, closed, deferUntil, lastActivity *time.Time
		var labels []string
		if err := rows.Scan(&id, &typ, &status, &title, &desc, &design, &acc, &notes, &priority, &assignee, &owner, &createdBy, &created, &updated,
			&started, &closed, &deferUntil, &closeReason, &ephemeral, &wispType, &eventKind, &hook, &agentState, &lastActivity, &parent, &labels); err != nil {
			rows.Close()
			return Diff{}, fmt.Errorf("migrate: verify issues: %w", err)
		}
		got[id] = issueFingerprint([]string{id, typ, status, title, desc, design, acc, notes, fmt.Sprint(priority), assignee, owner, createdBy,
			us(created), us(updated), usPtr(started), usPtr(closed), usPtr(deferUntil), closeReason, fmt.Sprint(ephemeral), wispType,
			eventKind, hook, agentState, usPtr(lastActivity), parent}, labels)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Diff{}, fmt.Errorf("migrate: verify issues: %w", err)
	}
	if len(got) != len(want) {
		p.add("issue count", "issues: %d in the tracker, %d in the dataset", len(got), len(want))
	}
	for id, w := range want {
		g, ok := got[id]
		switch {
		case !ok:
			p.add("missing issue", "issue %s is missing from the tracker", id)
		case g != w:
			p.add("changed issue", "issue %s differs:\n  dolt:    %s\n  tracker: %s", id, strings.ReplaceAll(w, "\x1f", "|"), strings.ReplaceAll(g, "\x1f", "|"))
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			p.add("extra issue", "issue %s is in the tracker but not in the dataset", id)
		}
	}

	// Links: the same set of (issue, target, type).
	wantDeps := map[string]bool{}
	for _, d := range deps {
		if _, ok := want[d.IssueID]; ok && d.DependsOn != "" {
			wantDeps[d.IssueID+"\x1f"+d.DependsOn+"\x1f"+d.Type] = true
		}
	}
	gotDeps := map[string]bool{}
	drows, err := q.Query(ctx, "SELECT d.issue_id, d.depends_on, d.type FROM tracker.dependencies d JOIN tracker.issues i ON i.id = d.issue_id WHERE i.rig = ANY($1)", rigs)
	if err != nil {
		return Diff{}, fmt.Errorf("migrate: verify dependencies: %w", err)
	}
	for drows.Next() {
		var a, b, c string
		if err := drows.Scan(&a, &b, &c); err != nil {
			drows.Close()
			return Diff{}, fmt.Errorf("migrate: verify dependencies: %w", err)
		}
		gotDeps[a+"\x1f"+b+"\x1f"+c] = true
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return Diff{}, fmt.Errorf("migrate: verify dependencies: %w", err)
	}
	diffSets(&p, "dependencies", wantDeps, gotDeps)

	// Comments and events, scoped to this dataset's rigs. A failed query
	// aborts: a zero that came from an error could coincide with an expected
	// zero and read as verified.
	commentsWant := 0
	for _, c := range ds.Comments {
		if _, ok := want[c.IssueID]; ok {
			commentsWant++
		}
	}
	for _, c := range []struct {
		name, sql string
		want      int
	}{
		{"comments", "SELECT count(*) FROM tracker.comments c JOIN tracker.issues i ON i.id = c.issue_id WHERE i.rig = ANY($1)", commentsWant},
		{"events", "SELECT count(*) FROM tracker.events WHERE split_part(issue_id, '-', 1) = ANY($1)", eventsInRigs(ds.Events, rigs)},
	} {
		var n int
		if err := q.QueryRow(ctx, c.sql, rigs).Scan(&n); err != nil {
			return Diff{}, fmt.Errorf("migrate: verify %s: %w", c.name, err)
		}
		if n != c.want {
			p.add("count "+c.name, "%s: %d in the tracker, %d expected", c.name, n, c.want)
		}
	}

	// Rig configuration is exact: the rig's namespaced keys, nothing missing
	// and nothing extra. Memories, types and statuses are town-wide and shared
	// by rigs, so each of this dataset's entries must be present with its value.
	cfg, kv := splitConfig(ds.Config)
	rig := primaryRig(ds)
	wantCfg := map[string]string{}
	for k, v := range cfg {
		wantCfg[configKey(rig, k)] = v
	}
	cfgSQL := "SELECT key, value FROM tracker.config"
	var cfgArgs []any
	if rig != "" {
		cfgSQL += " WHERE starts_with(key, $1)"
		cfgArgs = []any{configKey(rig, "")}
	}
	haveCfg, err := readPairs(ctx, q, cfgSQL, cfgArgs...)
	if err != nil {
		return Diff{}, fmt.Errorf("migrate: verify config: %w", err)
	}
	for k, v := range wantCfg {
		if hv, ok := haveCfg[k]; !ok || hv != v {
			p.add("config", "config %q is %q in the tracker, want %q", k, hv, v)
		}
	}
	for k := range haveCfg {
		if _, ok := wantCfg[k]; !ok {
			p.add("config extra", "config %q is in the tracker but not in the dataset", k)
		}
	}
	haveKV, err := readPairs(ctx, q, "SELECT key, value FROM tracker.kv")
	if err != nil {
		return Diff{}, fmt.Errorf("migrate: verify kv: %w", err)
	}
	for k, v := range kv {
		if hv, ok := haveKV[k]; !ok || hv != v {
			p.add("kv", "memory %q is %q in the tracker, want %q", k, hv, v)
		}
	}
	types, err := readPairs(ctx, q, "SELECT name, name FROM tracker.custom_types")
	if err != nil {
		return Diff{}, fmt.Errorf("migrate: verify custom types: %w", err)
	}
	for _, t := range ds.CustomTypes {
		if _, ok := types[t]; !ok {
			p.add("custom_types", "custom type %q is missing from the tracker", t)
		}
	}
	statuses, err := readPairs(ctx, q, "SELECT name, name FROM tracker.custom_statuses")
	if err != nil {
		return Diff{}, fmt.Errorf("migrate: verify custom statuses: %w", err)
	}
	for _, st := range ds.CustomStatuses {
		if _, ok := statuses[st.Name]; !ok {
			p.add("custom_statuses", "custom status %q is missing from the tracker", st.Name)
		}
	}
	// Child counters are keyed by parent id, so they are scoped by rig like
	// events: exact for this dataset's rigs.
	haveCtr, err := readPairs(ctx, q, "SELECT parent_id, last_child::text FROM tracker.child_counters WHERE split_part(parent_id, '-', 1) = ANY($1)", rigs)
	if err != nil {
		return Diff{}, fmt.Errorf("migrate: verify child counters: %w", err)
	}
	wantCtr := 0
	for parent, last := range ds.ChildCounters {
		if !inRigs(parent, rigs) {
			continue
		}
		wantCtr++
		if got, _ := strconv.Atoi(haveCtr[parent]); got < last {
			p.add("child_counters", "child counter of %s is %q in the tracker, want at least %d", parent, haveCtr[parent], last)
		}
	}
	if len(haveCtr) != wantCtr {
		p.add("child_counters count", "child counters of these rigs: %d in the tracker, %d expected", len(haveCtr), wantCtr)
	}
	return p.diff(), nil
}

// readPairs runs a two-column text query into a map.
func readPairs(ctx context.Context, q Querier, sql string, args ...any) (map[string]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func diffSets(p *problems, what string, want, got map[string]bool) {
	for k := range want {
		if !got[k] {
			p.add(what+" missing", "%s: %q is missing from the tracker", what, strings.ReplaceAll(k, "\x1f", "|"))
		}
	}
	for k := range got {
		if !want[k] {
			p.add(what+" extra", "%s: %q is in the tracker but not in the dataset", what, strings.ReplaceAll(k, "\x1f", "|"))
		}
	}
}

//go:build integration

package store

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// seedDependentsAmongManyPlainLinks stages the table the reverse read of gt-t
// is checked on: a dependent with 20000 plain links to ids that are not issues,
// beside a handful of external links to other targets and to gt-t.
func seedDependentsAmongManyPlainLinks(t *testing.T) (context.Context, *pgxpool.Pool, *Postgres) {
	t.Helper()
	ctx := api.WithActor(context.Background(), "town/crew/dipper")
	pool := pgtest.New(t, "migrated")
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown')"); err != nil {
		t.Fatal(err)
	}
	p := NewPostgres(pool)
	for _, id := range []string{"gt-t", "gt-a", "gt-b", "gt-c", "gt-d", "gt-bulk"} {
		if _, err := p.CreateIssue(ctx, api.CreateParams{ID: id, Title: id}); err != nil {
			t.Fatal(err)
		}
	}
	links := []struct{ from, to, typ string }{
		{"gt-a", "gt-t", "blocks"},
		{"gt-b", "external:om:gt-t", "blocks"},
		{"gt-c", "external:om:wisp-gt-t", "blocks"},
		{"gt-d", "gt-t", "tracks"},
		{"gt-d", "external:om:gt-t", "blocks"},
	}
	for _, l := range links {
		if _, err := pool.Exec(ctx, "INSERT INTO tracker.dependencies (issue_id, depends_on, type) VALUES ($1, $2, $3)", l.from, l.to, l.typ); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tracker.dependencies (issue_id, depends_on, type)
SELECT 'gt-bulk', 'gt-plain-' || g, 'blocks' FROM generate_series(1, 20000) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tracker.dependencies (issue_id, depends_on, type)
SELECT 'gt-bulk', 'external:om:gt-other-' || g, 'blocks' FROM generate_series(1, 5) g`); err != nil {
		t.Fatal(err)
	}
	return ctx, pool, p
}

// The reverse read of an id answers an external:<rig>:<id> link as well as a
// plain one, and must not change its answer for a table of 20000 plain links:
// the bulk must add no dependent.
func TestDependentsOfExternalTargetAmongManyPlainLinks(t *testing.T) {
	ctx, _, p := seedDependentsAmongManyPlainLinks(t)

	got, err := p.GetIssues(ctx, []string{"gt-t"})
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]string, 0, len(got["gt-t"].Dependents))
	for _, d := range got["gt-t"].Dependents {
		rows = append(rows, d.ID+" "+d.DependencyType)
	}
	sort.Strings(rows)
	want := []string{"gt-a blocks", "gt-b blocks", "gt-d tracks"}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("Dependents of gt-t = %v, want %v: the plain link, the external link, and gt-d once at its plain relation; the near miss and the bulk are absent", rows, want)
	}
}

// The external branch of the reverse read is served by its partial index, not
// a scan of the 20000 plain links beside it. The plan is read after ANALYZE, so
// the planner has the table's real size.
func TestDependentsOfExternalTargetUseTheIndex(t *testing.T) {
	ctx, pool, _ := seedDependentsAmongManyPlainLinks(t)
	if _, err := pool.Exec(ctx, "ANALYZE tracker.dependencies"); err != nil {
		t.Fatal(err)
	}

	rows, err := pool.Query(ctx, "EXPLAIN "+fillDependentsSQL, []string{"gt-t"})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "dependencies_external_target_idx") {
		t.Errorf("the dependents read does not use dependencies_external_target_idx; plan:\n%s", plan.String())
	}
}

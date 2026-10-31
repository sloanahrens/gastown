//go:build integration

package store

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// The reverse read of an id answers an external:<rig>:<id> link as well as a
// plain one, and must not change its answer for a table of 20000 plain links.
// The seed is one dependent with 20000 plain links to ids that are not issues,
// beside a handful of external links to other targets: the answer must hold
// with the bulk present, and the bulk must add no dependent.
func TestDependentsOfExternalTargetAmongManyPlainLinks(t *testing.T) {
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

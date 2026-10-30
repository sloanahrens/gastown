package store

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
)

// carries reports whether args holds v: the arguments are []any, which
// slices.Contains cannot take.
func carries(args []any, v any) bool {
	for _, a := range args {
		if a == v {
			return true
		}
	}
	return false
}

// The list's WHERE clause is one pure function, so the bounds the wisp read
// leans on are pinned here without a database. What a bound means to a row is
// the contract suite's job: it runs against Postgres.

func TestListWhereAppliesBothAgeBoundsAsParameters(t *testing.T) {
	t.Parallel() // unit tier: pure
	closedBefore := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	createdBefore := time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)
	where, args, err := buildListWhere(api.ListFilter{
		Ephemeral:     true,
		Statuses:      []string{"closed"},
		ClosedBefore:  closedBefore,
		CreatedBefore: createdBefore,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, clause := range []string{"issues.closed_at <", "issues.created_at <"} {
		if !strings.Contains(where, clause) {
			t.Errorf("where = %q, want %q", where, clause)
		}
	}
	if !carries(args, closedBefore) || !carries(args, createdBefore) {
		t.Errorf("args = %v, want both bound instants by value", args)
	}
	if strings.Contains(where, "2026-") {
		t.Errorf("where = %q carries a value; only placeholders may reach the query", where)
	}
}

// Both reads pin the ephemeral flag as a parameter, so a wisp read can never
// return a durable issue and a durable read can never return a wisp.
func TestListWherePinsEphemeralInBothDirections(t *testing.T) {
	t.Parallel()
	for _, ephemeral := range []bool{true, false} {
		where, args, err := buildListWhere(api.ListFilter{Ephemeral: ephemeral}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(where, "issues.ephemeral = $1") || !carries(args, ephemeral) {
			t.Errorf("ephemeral=%v: where = %q args = %v, want the flag pinned by its own parameter", ephemeral, where, args)
		}
	}
}

func TestListWhereLeavesAZeroBoundOut(t *testing.T) {
	t.Parallel()
	where, _, err := buildListWhere(api.ListFilter{Ephemeral: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, clause := range []string{"closed_at <", "created_at <"} {
		if strings.Contains(where, clause) {
			t.Errorf("where = %q holds %q for a zero bound, which is no bound at all", where, clause)
		}
	}
}

func TestListQueryOrdersAndFetchesOneRowPastThePage(t *testing.T) {
	t.Parallel()
	q, _, err := buildListQuery(api.ListFilter{Ephemeral: true}, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(q, " ORDER BY priority, created_at, id LIMIT 3") {
		t.Errorf("query = %q, want the list order and one row past the two-row page", q)
	}
}

// The count is of the whole match, so it asks for no page: the same WHERE as
// the list, and no order or limit for a row to be dropped by.
func TestCountQueryIsTheListWhereWithNoPage(t *testing.T) {
	t.Parallel()
	f := api.ListFilter{Ephemeral: true, Statuses: []string{"closed"}, CreatedBefore: time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)}
	where, whereArgs, err := buildListWhere(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	q, args, err := buildCountQuery(f)
	if err != nil {
		t.Fatal(err)
	}
	if q != "SELECT count(*) FROM tracker.issues WHERE "+where {
		t.Errorf("count query = %q, want the list's WHERE clause and nothing after it", q)
	}
	if len(args) != len(whereArgs) {
		t.Errorf("count args = %v, want the list's %v", args, whereArgs)
	}
	for _, clause := range []string{"ORDER BY", "LIMIT"} {
		if strings.Contains(q, clause) {
			t.Errorf("count query = %q holds %q, which would count a page rather than the match", q, clause)
		}
	}
}

func TestListWhereAddsTheCursorAfterTheFilter(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 30, 11, 0, 0, 0, time.UTC)
	where, args, err := buildListWhere(api.ListFilter{Ephemeral: true}, &listCursor{Priority: 2, CreatedAt: at, ID: "gt-b"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(where, "(issues.priority, issues.created_at, issues.id) >") {
		t.Errorf("where = %q, want the cursor comparison", where)
	}
	if !carries(args, "gt-b") || !carries(args, at) {
		t.Errorf("args = %v, want the cursor's id and instant", args)
	}
}

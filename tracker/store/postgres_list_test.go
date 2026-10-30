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

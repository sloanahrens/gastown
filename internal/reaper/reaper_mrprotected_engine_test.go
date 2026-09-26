package reaper

import (
	"context"
	"fmt"
	"testing"
	"time"

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/memory"
	gmssql "github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
)

// TestMRProtectedJoinAgainstRealEngine runs mrProtectedJoin's actual generated
// SQL text against an in-process MySQL-compatible engine (go-mysql-server),
// rather than the fake driver the rest of this package uses.
//
// The fake driver in this file pattern-matches the query string and computes
// its answer from a hand-written Go re-implementation of the intended
// semantics (mrProtectedLocked), so it cannot catch a bug in the SQL text
// itself. That is exactly how the ON-vs-WHERE regression shipped with 100%
// green tests: cleanupWispRequestedWhere's TTL predicate landed in the ON
// clause of a LEFT JOIN, which never filters rows, so every wisp in the
// database became "protected" and Scan/Reap stopped reaping entirely — while
// every fake-driver test kept passing because it never executed the text
// (gt-apam rejection). This test executes the text.
func TestMRProtectedJoinAgainstRealEngine(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	maxProtection := 24 * time.Hour
	cutoff := now.Add(-maxProtection)

	type wisp struct {
		id        string
		status    string
		createdAt time.Time
		labels    []string
		// labelSetAt, if non-zero, adds a wisp_events label_set row anchoring
		// the state:merge-requested TTL to this time instead of createdAt.
		labelSetAt time.Time
	}

	wisps := []wisp{
		// Unconditionally protected: labeled gt:merge-request.
		{id: "mr-open", status: "open", createdAt: now.Add(-100 * time.Hour), labels: []string{"gt:merge-request"}},

		// Cleanup + state:merge-requested, anchored by a fresh wisp_events row
		// -> still within the TTL window -> protected.
		{id: "cleanup-fresh", status: "open", createdAt: now.Add(-100 * time.Hour),
			labels: []string{"cleanup", "state:merge-requested"}, labelSetAt: now.Add(-1 * time.Hour)},

		// Cleanup + state:merge-requested, anchored by a stale wisp_events row
		// -> past the TTL -> reapable.
		{id: "cleanup-expired", status: "open", createdAt: now.Add(-100 * time.Hour),
			labels: []string{"cleanup", "state:merge-requested"}, labelSetAt: now.Add(-100 * time.Hour)},

		// Cleanup + state:merge-requested, no wisp_events row -> anchor falls
		// back to created_at, which is recent -> protected.
		{id: "cleanup-fallback-fresh", status: "open", createdAt: now.Add(-1 * time.Hour),
			labels: []string{"cleanup", "state:merge-requested"}},

		// Same fallback, but created_at is past the TTL -> reapable.
		{id: "cleanup-fallback-expired", status: "open", createdAt: now.Add(-100 * time.Hour),
			labels: []string{"cleanup", "state:merge-requested"}},

		// Cleanup-labeled only, no state:merge-requested -> never protected by
		// this branch.
		{id: "cleanup-only", status: "open", createdAt: now.Add(-1 * time.Hour), labels: []string{"cleanup"}},

		// A plain wisp with no labels at all. Under the ON-clause bug this
		// wisp still showed up in mr_protected (every row matched), which is
		// the regression this test exists to catch.
		{id: "plain-open", status: "open", createdAt: now.Add(-1 * time.Hour)},
	}

	ctx, engine := newWispTestEngine(t)
	for _, w := range wisps {
		insertWisp(t, ctx, engine, w.id, w.status, w.createdAt)
		for _, label := range w.labels {
			insertWispLabel(t, ctx, engine, w.id, label)
		}
		if !w.labelSetAt.IsZero() {
			insertWispLabelSetEvent(t, ctx, engine, w.id, "state:merge-requested", w.labelSetAt)
		}
	}

	joinClause, whereCondition := mrProtectedJoin(maxProtection, cutoff)
	query := fmt.Sprintf("SELECT w.id FROM wisps w %s WHERE %s ORDER BY w.id", joinClause, whereCondition)

	got := runIDQuery(t, ctx, engine, query)

	want := []string{
		"cleanup-expired",
		"cleanup-fallback-expired",
		"cleanup-only",
		"plain-open",
	}
	assertIDSlicesEqual(t, got, want)
}

// newWispTestEngine builds an in-memory go-mysql-server engine with the
// wisps/wisp_labels/wisp_events tables mrProtectedJoin's SQL references.
func newWispTestEngine(t *testing.T) (*gmssql.Context, *sqle.Engine) {
	t.Helper()

	db := memory.NewDatabase("wisptest")
	pro := memory.NewDBProvider(db)
	session := memory.NewSession(gmssql.NewBaseSession(), pro)
	ctx := gmssql.NewContext(context.Background(), gmssql.WithSession(session))
	ctx.SetCurrentDatabase("wisptest")

	db.AddTable("wisps", memory.NewTable(ctx, db, "wisps", gmssql.NewPrimaryKeySchema(gmssql.Schema{
		{Name: "id", Type: types.Text, Nullable: false, Source: "wisps"},
		{Name: "status", Type: types.Text, Nullable: false, Source: "wisps"},
		{Name: "created_at", Type: types.Datetime, Nullable: false, Source: "wisps"},
	}), db.GetForeignKeyCollection()))

	db.AddTable("wisp_labels", memory.NewTable(ctx, db, "wisp_labels", gmssql.NewPrimaryKeySchema(gmssql.Schema{
		{Name: "issue_id", Type: types.Text, Nullable: false, Source: "wisp_labels"},
		{Name: "label", Type: types.Text, Nullable: false, Source: "wisp_labels"},
	}), db.GetForeignKeyCollection()))

	db.AddTable("wisp_events", memory.NewTable(ctx, db, "wisp_events", gmssql.NewPrimaryKeySchema(gmssql.Schema{
		{Name: "issue_id", Type: types.Text, Nullable: false, Source: "wisp_events"},
		{Name: "event_type", Type: types.Text, Nullable: false, Source: "wisp_events"},
		{Name: "old_value", Type: types.Text, Nullable: true, Source: "wisp_events"},
		{Name: "created_at", Type: types.Datetime, Nullable: false, Source: "wisp_events"},
	}), db.GetForeignKeyCollection()))

	engine := sqle.NewDefault(pro)
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("closing test engine: %v", err)
		}
	})
	return ctx, engine
}

func insertWisp(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, id, status string, createdAt time.Time) {
	t.Helper()
	mustExec(t, ctx, engine, fmt.Sprintf(
		"INSERT INTO wisps (id, status, created_at) VALUES ('%s', '%s', '%s')",
		id, status, createdAt.UTC().Format("2006-01-02 15:04:05")))
}

func insertWispLabel(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, issueID, label string) {
	t.Helper()
	mustExec(t, ctx, engine, fmt.Sprintf(
		"INSERT INTO wisp_labels (issue_id, label) VALUES ('%s', '%s')", issueID, label))
}

func insertWispLabelSetEvent(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, issueID, oldValue string, createdAt time.Time) {
	t.Helper()
	mustExec(t, ctx, engine, fmt.Sprintf(
		"INSERT INTO wisp_events (issue_id, event_type, old_value, created_at) VALUES ('%s', 'label_set', '%s', '%s')",
		issueID, oldValue, createdAt.UTC().Format("2006-01-02 15:04:05")))
}

func mustExec(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, query string) {
	t.Helper()
	_, iter, _, err := engine.Query(ctx, query)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if _, err := gmssql.RowIterToRows(ctx, iter); err != nil {
		t.Fatalf("draining %q: %v", query, err)
	}
}

func runIDQuery(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, query string) []string {
	t.Helper()
	_, iter, _, err := engine.Query(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	rows, err := gmssql.RowIterToRows(ctx, iter)
	if err != nil {
		t.Fatalf("reading rows for %q: %v", query, err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		id, ok := row[0].(string)
		if !ok {
			t.Fatalf("row id is %T, want string: %v", row[0], row[0])
		}
		ids = append(ids, id)
	}
	return ids
}

func assertIDSlicesEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("candidate ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate ids = %v, want %v", got, want)
		}
	}
}

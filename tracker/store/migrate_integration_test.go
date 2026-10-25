//go:build integration

package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

func tables(t *testing.T, p *pgxpool.Pool) []string {
	t.Helper()
	rows, err := p.Query(context.Background(), "SELECT table_name FROM information_schema.tables WHERE table_schema = 'tracker' AND table_type = 'BASE TABLE' ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func exec(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// Integration tier: a real postgres, one clone per case (~60 ms).
func TestMigrateAppliesEverythingAndIsIdempotent(t *testing.T) {
	t.Parallel()
	p := pgtest.New(t, "empty")
	ctx := context.Background()
	if lvl, err := CurrentLevel(ctx, p); err != nil || lvl != 0 {
		t.Fatalf("CurrentLevel on an empty database = %d, %v; want 0, nil", lvl, err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, p); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}
	if lvl, err := CurrentLevel(ctx, p); err != nil || lvl != SchemaLevel() {
		t.Errorf("CurrentLevel = %d, %v; want %d", lvl, err, SchemaLevel())
	}
	want := []string{"child_counters", "comments", "config", "custom_statuses", "custom_types", "dependencies", "events", "issues", "kv", "labels", "rigs", "schema_migrations", "tokens"}
	if got := tables(t, p); !slices.Equal(got, want) {
		t.Errorf("tables = %v, want %v (mail and receipt tables are a later migration)", got, want)
	}
}

func TestMigrateRefusesANewerDatabase(t *testing.T) {
	t.Parallel()
	p := pgtest.New(t, "migrated")
	exec(t, p, "INSERT INTO tracker.schema_migrations (version, name) VALUES (999, 'from_the_future')")
	err := Migrate(context.Background(), p)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("Migrate on a newer database = %v, want ErrSchemaTooNew", err)
	}
}

func TestMigrateRefusesARenamedMigration(t *testing.T) {
	t.Parallel()
	p := pgtest.New(t, "migrated")
	exec(t, p, "UPDATE tracker.schema_migrations SET name = 'something_else' WHERE version = 1")
	err := Migrate(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "version 1") {
		t.Errorf("Migrate with a renamed version 1 = %v, want an error naming version 1", err)
	}
}

func TestConcurrentMigratorsApplyEachMigrationOnce(t *testing.T) {
	t.Parallel()
	p := pgtest.New(t, "empty")
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = Migrate(context.Background(), p)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("migrator %d: %v", i, err)
		}
	}
	var n int
	if err := p.QueryRow(context.Background(), "SELECT count(*) FROM tracker.schema_migrations").Scan(&n); err != nil || n != SchemaLevel() {
		t.Errorf("schema_migrations has %d rows, %v; want %d (one per migration)", n, err, SchemaLevel())
	}
}

// The schema's promises from the design spec, each as behavior.
func TestSchemaShape(t *testing.T) {
	t.Parallel()
	p := pgtest.New(t, "migrated")
	ctx := context.Background()

	t.Run("no stored is_blocked", func(t *testing.T) {
		var n int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema='tracker' AND table_name='issues' AND column_name='is_blocked'").Scan(&n); err != nil || n != 0 {
			t.Errorf("issues has an is_blocked column (%d, %v): ready and blocked compute blocking", n, err)
		}
	})

	t.Run("the hot-path indexes are partial", func(t *testing.T) {
		for _, name := range []string{"issues_active_idx", "issues_assignee_idx", "issues_parent_idx", "issues_ephemeral_idx"} {
			var def string
			if err := p.QueryRow(ctx, "SELECT indexdef FROM pg_indexes WHERE schemaname='tracker' AND indexname=$1", name).Scan(&def); err != nil {
				t.Errorf("index %s: %v", name, err)
				continue
			}
			if !strings.Contains(def, "WHERE") {
				t.Errorf("index %s is not partial: %s", name, def)
			}
		}
	})

	exec(t, p, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown'), ('hq', 'town')")

	t.Run("an id must start with its rig prefix", func(t *testing.T) {
		_, err := p.Exec(ctx, "INSERT INTO tracker.issues (id, rig, issue_type, status, title) VALUES ('gt-1', 'hq', 'task', 'open', 'x')")
		if err == nil {
			t.Error("inserted gt-1 under rig hq; the id prefix check did not fire")
		}
	})

	exec(t, p, "INSERT INTO tracker.issues (id, rig, issue_type, status, title) VALUES ('gt-a', 'gt', 'task', 'open', 'a'), ('gt-b', 'gt', 'task', 'open', 'b')")
	exec(t, p, "INSERT INTO tracker.labels (issue_id, label) VALUES ('gt-a', 'x')")
	exec(t, p, "INSERT INTO tracker.comments (issue_id, author, text) VALUES ('gt-a', 'me', 'hi')")
	exec(t, p, "INSERT INTO tracker.dependencies (issue_id, depends_on, type) VALUES ('gt-a', 'gt-b', 'blocks'), ('gt-a', 'external:hq:hq-9', 'tracks')")
	exec(t, p, "INSERT INTO tracker.events (op, issue_id, actor) VALUES ('created', 'gt-a', 'me')")
	exec(t, p, "DELETE FROM tracker.issues WHERE id = 'gt-a'")

	count := func(sql string) int {
		var n int
		if err := p.QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t.Run("deleting an issue removes its labels, comments and outgoing links", func(t *testing.T) {
		for _, tbl := range []string{"labels", "comments", "dependencies"} {
			if n := count("SELECT count(*) FROM tracker." + tbl); n != 0 {
				t.Errorf("%s has %d rows after its issue was deleted, want 0", tbl, n)
			}
		}
	})
	t.Run("an issue's events outlive the issue", func(t *testing.T) {
		if n := count("SELECT count(*) FROM tracker.events WHERE issue_id = 'gt-a'"); n != 1 {
			t.Errorf("events for the deleted issue = %d, want 1 (no cascade)", n)
		}
	})
	t.Run("an external dependency target needs no row", func(t *testing.T) {
		exec(t, p, "INSERT INTO tracker.dependencies (issue_id, depends_on, type) VALUES ('gt-b', 'external:om:om-7', 'tracks')")
	})
	t.Run("event seq is monotonic", func(t *testing.T) {
		exec(t, p, "INSERT INTO tracker.events (op, issue_id) VALUES ('updated', 'gt-b'), ('closed', 'gt-b')")
		rows, err := p.Query(ctx, "SELECT seq FROM tracker.events ORDER BY seq")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		prev := int64(0)
		for rows.Next() {
			var s int64
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			if s <= prev {
				t.Errorf("seq %d not greater than %d", s, prev)
			}
			prev = s
		}
	})
}

// A failed query is not "no schema yet": only a missing table means level 0.
func TestCurrentLevelDoesNotMistakeAFailureForNoSchema(t *testing.T) {
	t.Parallel()
	p := pgtest.New(t, "migrated")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // every query on this context fails
	if lvl, err := CurrentLevel(ctx, p); err == nil {
		t.Errorf("CurrentLevel with a canceled context = %d, nil; want an error, not level 0", lvl)
	}
}

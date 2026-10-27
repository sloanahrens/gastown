//go:build integration

package store

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// pageReadSQL identifies the snapshot's page read: the only query that pages
// the issues table by the lists' sort key. pgx traces BEGIN too, so the
// interleave point cannot be "the first statement"; it is the end of this one.
const pageReadSQL = "ORDER BY priority, created_at, id LIMIT"

// interleaveTracer runs a write when the snapshot's page read ends. pgx calls
// TraceQueryEnd once the rows are closed, so the write commits after the page
// read and before the snapshot's next read, with no seam in the store and no
// sleep: two clients, one reading and one writing, in a fixed order.
type interleaveTracer struct {
	mu    sync.Mutex
	fn    func() error
	armed bool
	ran   bool
	err   error
	sql   string
}

func (t *interleaveTracer) arm(fn func() error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fn, t.armed = fn, true
}

func (t *interleaveTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.sql = data.SQL
	t.mu.Unlock()
	return ctx
}

func (t *interleaveTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
	t.mu.Lock()
	if !t.armed || t.ran || !strings.Contains(t.sql, pageReadSQL) {
		t.mu.Unlock()
		return
	}
	t.ran = true
	fn := t.fn
	t.mu.Unlock()
	err := fn()
	t.mu.Lock()
	t.err = err
	t.mu.Unlock()
}

func (t *interleaveTracer) result() (ran bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ran, t.err
}

func snapshotTestIDs(snap api.TownSnapshot) []string {
	out := make([]string, len(snap.Issues))
	for i, is := range snap.Issues {
		out[i] = is.ID
	}
	return out
}

// Integration tier: a real Postgres clone and two clients. A write commits
// between the snapshot's page read and its counts read; with the one
// repeatable-read transaction the page and the counts both miss it, where a
// read per statement would count an issue the page does not carry.
func TestTownSnapshotIsOneMomentWhileAWriteCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.New(t, "migrated")
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown')"); err != nil {
		t.Fatal(err)
	}
	writer := NewPostgres(pool)

	// The reader's pool carries the interleave tracer; the writer's does not.
	cfg := pool.Config()
	tracer := &interleaveTracer{}
	cfg.ConnConfig.Tracer = tracer
	readPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("reader pool: %v", err)
	}
	t.Cleanup(readPool.Close)
	reader := NewPostgres(readPool)

	first, err := writer.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "first", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	tracer.arm(func() error {
		_, err := writer.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "late", Priority: 1})
		return err
	})
	snap, err := reader.TownSnapshot(ctx, api.SnapshotRequest{})
	if err != nil {
		t.Fatalf("TownSnapshot: %v", err)
	}
	if ran, err := tracer.result(); !ran || err != nil {
		t.Fatalf("the write between the snapshot's reads ran=%v err=%v, want it committed", ran, err)
	}
	if snap.At.IsZero() {
		t.Error("At is zero; the snapshot must say when it read")
	}
	if snap.Truncated || len(snap.Issues) != 1 || snap.Issues[0].ID != first.ID {
		t.Errorf("page = %v truncated=%v, want only %s: a write committed after the page read must not appear", snapshotTestIDs(snap), snap.Truncated, first.ID)
	}
	if snap.Counts.Total != 1 || snap.Counts.ByStatus["open"] != 1 || snap.Counts.ByRig["gt"] != 1 {
		t.Errorf("counts = %+v, want 1 open gt issue: the counts read the same moment as the page", snap.Counts)
	}

	// The interleaved write did land: a later snapshot sees it.
	after, err := reader.TownSnapshot(ctx, api.SnapshotRequest{})
	if err != nil {
		t.Fatalf("TownSnapshot again: %v", err)
	}
	if after.Counts.Total != 2 || len(after.Issues) != 2 {
		t.Errorf("later snapshot = %v, total %d; want both issues, proving the interleaved write committed", snapshotTestIDs(after), after.Counts.Total)
	}
	if after.At.Before(snap.At) {
		t.Errorf("later snapshot At = %v, before the first %v; the moment must not go backwards", after.At, snap.At)
	}
}

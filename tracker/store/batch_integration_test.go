//go:build integration

package store

import (
	"bytes"
	"context"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// A batch create takes a fresh id when its generated one collides, as a single
// create does, rather than failing an operation the caller cannot fix. This
// test fixes the id source, so it does not run parallel with the others.
func TestBatchCreateRetriesACollidingGeneratedID(t *testing.T) {
	ctx := api.WithActor(context.Background(), "town/crew/dipper")
	pool := pgtest.New(t, "migrated")
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown')"); err != nil {
		t.Fatal(err)
	}
	p := NewPostgres(pool)
	if _, err := p.CreateIssue(ctx, api.CreateParams{ID: "gt-aaaaa", Title: "taken"}); err != nil {
		t.Fatal(err)
	}
	old := idSource
	t.Cleanup(func() { idSource = old })
	// Base-36 digits 10 and 11 spell aaaaa and bbbbb: the first draw repeats the
	// id the store already holds, the second one is free.
	draws := append(bytes.Repeat([]byte{10}, idAttempts+1), bytes.Repeat([]byte{11}, idAttempts+1)...)
	idSource = bytes.NewReader(draws)

	res, err := p.Batch(ctx, api.BatchRequest{Operations: []api.BatchOperation{
		{Op: api.BatchOpCreate, Create: &api.CreateParams{Rig: "gt", Title: "minted"}},
	}})
	if err != nil {
		t.Fatalf("Batch = %v, want the colliding id retried", err)
	}
	if id := res.Results[0].IDs[0]; id != "gt-bbbbb" {
		t.Errorf("created %q, want gt-bbbbb: the first draw, gt-aaaaa, was taken", id)
	}
}

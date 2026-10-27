//go:build integration

package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// claimRaceBound bounds one round of racing claims, so a deadlocked server
// fails the test instead of hanging it.
const claimRaceBound = 30 * time.Second

// waitClaims waits for the claim goroutines, failing the test if they are not
// done within claimRaceBound.
func waitClaims(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(claimRaceBound):
		t.Fatalf("the racing claims did not finish within %s", claimRaceBound)
	}
}

// assignClients returns one client per actor, over a real server and a
// migrated Postgres clone, so each call crosses the wire as a seat's would.
func assignClients(t *testing.T, actors ...string) map[string]*client.Client {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.New(t, "migrated")
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown'), ('hq', 'town')"); err != nil {
		t.Fatal(err)
	}
	pg := store.NewPostgres(pool)
	srv := httptest.NewServer(New(Config{Store: pg, Auth: pg, Health: pg, SchemaLevel: store.SchemaLevel()}).Handler())
	t.Cleanup(srv.Close)
	out := make(map[string]*client.Client, len(actors))
	for _, actor := range actors {
		tok, err := pg.IssueToken(ctx, actor, nil)
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.New(client.Config{BaseURL: srv.URL, Token: tok, SchemaLevel: store.SchemaLevel()})
		if err != nil {
			t.Fatal(err)
		}
		out[actor] = c
	}
	return out
}

// Integration tier: the guard is one row-locked step in the store and one
// refusal kind on the wire, so two seats racing for an issue cannot both hold
// it.
func TestTwoClientsCannotBothClaimOneIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Both seats wait on one barrier and start together; no sleep orders them.
	c := assignClients(t, "seat-a", "seat-b")
	is, err := c["seat-a"].Create(ctx, api.CreateParams{Rig: "gt", Title: "contested"})
	if err != nil {
		t.Fatal(err)
	}

	seats := []string{"seat-a", "seat-b"}
	errs := make([]error, len(seats))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, actor := range seats {
		wg.Add(1)
		go func(i int, actor string) {
			defer wg.Done()
			<-start
			errs[i] = c[actor].Claim(ctx, is.ID)
		}(i, actor)
	}
	close(start)
	waitClaims(t, &wg)

	winner, losers := "", 0
	for i, err := range errs {
		switch {
		case err == nil && winner == "":
			winner = seats[i]
		case errors.Is(err, api.ErrGuardFailed):
			losers++
		default:
			t.Fatalf("%s: claim = %v, want one nil and one api.ErrGuardFailed", seats[i], err)
		}
	}
	if winner == "" || losers != 1 {
		t.Fatalf("claims = %v, want exactly one winner", errs)
	}
	got, err := c["seat-a"].Show(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != winner || got.Status != "in_progress" {
		t.Errorf("after the race: assignee %q status %q, want %s and in_progress", got.Assignee, got.Status, winner)
	}
}

// Integration tier: a seat that does not hold an issue can neither release nor
// transfer it, and the refusals leave the row as it was.
func TestReleaseAndTransferRefuseASeatThatDoesNotHoldTheIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := assignClients(t, "seat-a", "seat-b")
	is, err := c["seat-a"].Create(ctx, api.CreateParams{Rig: "gt", Title: "held by a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c["seat-a"].Claim(ctx, is.ID); err != nil {
		t.Fatal(err)
	}

	if err := c["seat-b"].Release(ctx, is.ID); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("release by a seat that does not hold it = %v, want api.ErrGuardFailed", err)
	}
	if err := c["seat-b"].Transfer(ctx, is.ID, "open", "seat-b"); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("transfer by a seat that does not hold it = %v, want api.ErrGuardFailed", err)
	}
	got, err := c["seat-b"].Show(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "seat-a" || got.Status != "in_progress" {
		t.Errorf("a refused release or transfer wrote: assignee %q status %q, want seat-a and in_progress", got.Assignee, got.Status)
	}

	if err := c["seat-a"].Release(ctx, is.ID); err != nil {
		t.Fatalf("the holder's release = %v, want nil", err)
	}
	if got, err = c["seat-a"].Show(ctx, is.ID); err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "" || got.Status != "open" {
		t.Errorf("after release: assignee %q status %q, want empty and open", got.Assignee, got.Status)
	}
}

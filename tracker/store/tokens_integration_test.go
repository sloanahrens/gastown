//go:build integration

package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// Integration tier: real postgres, one clone per case.
func TestIssuedTokenResolvesToItsActorAndIsStoredHashed(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t, "migrated")
	pg := NewPostgres(pool)
	ctx := context.Background()
	token, err := pg.IssueToken(ctx, "town/crew/dipper", nil)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := pg.ActorForToken(ctx, token)
	if err != nil || actor != "town/crew/dipper" {
		t.Fatalf("ActorForToken = %q, %v; want the actor it was issued to", actor, err)
	}
	var stored []byte
	if err := pool.QueryRow(ctx, "SELECT token_hash FROM tracker.tokens").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, HashToken(token)) || bytes.Contains(stored, []byte(token)) {
		t.Error("the tokens table must hold the hash of the token, never the token")
	}
	var plaintext int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM tracker.tokens t WHERE t::text LIKE '%' || $1 || '%'", token).Scan(&plaintext); err != nil || plaintext != 0 {
		t.Errorf("the token appears in %d tokens rows (%v); it must appear in none", plaintext, err)
	}
}

func TestUnknownRevokedAndExpiredTokensAreRefused(t *testing.T) {
	t.Parallel()
	pg := NewPostgres(pgtest.New(t, "migrated"))
	ctx := context.Background()
	if _, err := pg.ActorForToken(ctx, "trk_nope"); !errors.Is(err, api.ErrUnknownToken) {
		t.Errorf("unknown token: %v, want api.ErrUnknownToken", err)
	}
	revoked, _ := pg.IssueToken(ctx, "a", nil)
	if ok, err := pg.RevokeToken(ctx, revoked); err != nil || !ok {
		t.Fatalf("RevokeToken = %v, %v; want true", ok, err)
	}
	if _, err := pg.ActorForToken(ctx, revoked); !errors.Is(err, api.ErrUnknownToken) {
		t.Errorf("revoked token: %v, want api.ErrUnknownToken", err)
	}
	if ok, _ := pg.RevokeToken(ctx, revoked); ok {
		t.Error("revoking an already revoked token reported success")
	}
	past := time.Now().Add(-time.Hour)
	expired, _ := pg.IssueToken(ctx, "b", &past)
	if _, err := pg.ActorForToken(ctx, expired); !errors.Is(err, api.ErrUnknownToken) {
		t.Errorf("expired token: %v, want api.ErrUnknownToken", err)
	}
	if _, err := pg.IssueToken(ctx, "", nil); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("a token with no actor: %v, want api.ErrInvalid", err)
	}
}

func TestPingAndWriteProbeSucceedOnAPrimaryAndFailOnADeadPool(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t, "migrated")
	pg := NewPostgres(pool)
	ctx := context.Background()
	if err := pg.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if err := pg.WriteProbe(ctx); err != nil {
		t.Errorf("WriteProbe on a primary: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM tracker.kv").Scan(&n); err != nil || n != 0 {
		t.Errorf("WriteProbe left %d kv rows (%v); it must leave nothing", n, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := pg.Ping(cctx); err == nil {
		t.Error("Ping with a canceled context succeeded")
	}
	if err := pg.WriteProbe(cctx); err == nil {
		t.Error("WriteProbe with a canceled context succeeded")
	}
}

// A database that refuses writes must fail the write probe: that is what
// /readyz exists to say. default_transaction_read_only models a read-only
// primary; a standby fails the same way.
func TestWriteProbeFailsOnAReadOnlyDatabase(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t, "migrated")
	ctx := context.Background()
	db := pool.Config().ConnConfig.Database
	if _, err := pool.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{db}.Sanitize()+" SET default_transaction_read_only = on"); err != nil {
		t.Fatal(err)
	}
	// New connections pick the setting up; open a pool that has none yet.
	ro, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ro.Close)
	pg := NewPostgres(ro)
	if err := pg.Ping(ctx); err != nil {
		t.Fatalf("Ping on a read-only database: %v (reads still work)", err)
	}
	if err := pg.WriteProbe(ctx); err == nil {
		t.Error("WriteProbe succeeded on a read-only database; /readyz would report ready")
	}
}

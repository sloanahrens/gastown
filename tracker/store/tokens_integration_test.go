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

// An issued token's id names exactly that row: revoking by id ends the one
// token it names and leaves the actor's other token live, and an id that names
// no live token is reported false rather than succeeded. The id is read from
// the row, because no read of the API returns it.
func TestRevokeTokenIDEndsOnlyThatToken(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t, "migrated")
	pg := NewPostgres(pool)
	ctx := context.Background()
	// The revoked token is the only one of the actor's with an expiry, so its
	// row is named by that rather than by its hash. The expiry is in the future,
	// so the token is live until it is revoked.
	future := time.Now().Add(time.Hour)
	revoked, err := pg.IssueToken(ctx, "town/crew/dipper", &future)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := pg.IssueToken(ctx, "town/crew/dipper", nil)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(ctx, "SELECT id::text FROM tracker.tokens WHERE actor = $1 AND expires_at IS NOT NULL", "town/crew/dipper").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.RevokeTokenID(ctx, id); err != nil || !ok {
		t.Fatalf("RevokeTokenID = %v, %v; want true", ok, err)
	}
	if _, err := pg.ActorForToken(ctx, revoked); !errors.Is(err, api.ErrUnknownToken) {
		t.Errorf("the token revoked by id = %v, want api.ErrUnknownToken", err)
	}
	if actor, err := pg.ActorForToken(ctx, kept); err != nil || actor != "town/crew/dipper" {
		t.Errorf("the actor's other token = %q, %v; want it still live", actor, err)
	}
	if ok, err := pg.RevokeTokenID(ctx, id); err != nil || ok {
		t.Errorf("revoking the same id twice = %v, %v; want false", ok, err)
	}
	for name, bad := range map[string]string{"not a uuid": "tok-nope", "no such uuid": "7f8d3c1e-0000-0000-0000-000000000000"} {
		if ok, err := pg.RevokeTokenID(ctx, bad); err != nil || ok {
			t.Errorf("RevokeTokenID(%s) = %v, %v; want false and no error", name, ok, err)
		}
	}
	if _, err := pg.RevokeTokenID(ctx, ""); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("RevokeTokenID with no id = %v, want api.ErrInvalid", err)
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

func TestIntegrationATokenHasAScopeAndTheDefaultIsSeat(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t, "migrated")
	pg := NewPostgres(pool)
	ctx := context.Background()
	seat, err := pg.IssueToken(ctx, "town/polecats/a", nil)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pg.IssueTokenScoped(ctx, "town/daemon", api.ScopeAdmin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := pg.Identify(ctx, seat); err != nil || id.Actor != "town/polecats/a" || id.Scope != api.ScopeSeat {
		t.Errorf("Identify(seat) = %+v, %v; want the actor and scope seat", id, err)
	}
	if id, err := pg.Identify(ctx, admin); err != nil || id.Actor != "town/daemon" || id.Scope != api.ScopeAdmin {
		t.Errorf("Identify(admin) = %+v, %v; want the actor and scope admin", id, err)
	}
	if _, err := pg.IssueTokenScoped(ctx, "x", "root", nil); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("IssueTokenScoped with scope root = %v, want api.ErrInvalid", err)
	}
	if _, err := pg.Identify(ctx, "trk_nope"); !errors.Is(err, api.ErrUnknownToken) {
		t.Errorf("Identify(unknown) = %v, want api.ErrUnknownToken", err)
	}
	// A revoked admin token is no admin token.
	if ok, err := pg.RevokeToken(ctx, admin); err != nil || !ok {
		t.Fatalf("RevokeToken = %v, %v", ok, err)
	}
	if _, err := pg.Identify(ctx, admin); !errors.Is(err, api.ErrUnknownToken) {
		t.Errorf("Identify(revoked admin) = %v, want api.ErrUnknownToken", err)
	}
}

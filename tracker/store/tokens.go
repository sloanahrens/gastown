package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// tokenRandomBytes is the entropy of an issued token.
const tokenRandomBytes = 32

// HashToken is how a token is stored: the SHA-256 of the whole token string.
// The token is high-entropy random, so a fast hash is enough; the point is
// that the database never holds a usable token.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// generateToken reads tokenRandomBytes from r and returns "trk_" plus their
// hex. A short read is an error: never a weaker token.
func generateToken(r io.Reader) (string, error) {
	b := make([]byte, tokenRandomBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("store: generate token: %w", err)
	}
	return "trk_" + hex.EncodeToString(b), nil
}

// IssueToken creates a seat token for actor, valid until expires (never when
// nil). See IssueTokenScoped.
func (p *Postgres) IssueToken(ctx context.Context, actor string, expires *time.Time) (string, error) {
	return p.IssueTokenScoped(ctx, actor, api.ScopeSeat, expires)
}

// IssueTokenScoped creates a token for actor with the given scope, valid until
// expires (never when nil), stores its hash, and returns the token. This is
// the only time the token exists in the clear: callers hand it to its owner and
// never log it.
func (p *Postgres) IssueTokenScoped(ctx context.Context, actor string, scope api.Scope, expires *time.Time) (string, error) {
	if actor == "" {
		return "", api.Invalid("a token needs an actor")
	}
	if !scope.Valid() {
		return "", api.Invalid("scope %q is not seat or admin", scope)
	}
	token, err := generateToken(rand.Reader)
	if err != nil {
		return "", err
	}
	if _, err := p.pool.Exec(ctx, "INSERT INTO tracker.tokens (actor, scope, token_hash, expires_at) VALUES ($1, $2, $3, $4)", actor, string(scope), HashToken(token), expires); err != nil {
		return "", fmt.Errorf("store: issue token: %w", err)
	}
	return token, nil
}

// Identify returns who token belongs to and its scope, or api.ErrUnknownToken
// when it is not known, was revoked, or has expired.
func (p *Postgres) Identify(ctx context.Context, token string) (api.Identity, error) {
	var id api.Identity
	var scope string
	err := p.pool.QueryRow(ctx,
		"SELECT actor, scope FROM tracker.tokens WHERE token_hash = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())",
		HashToken(token)).Scan(&id.Actor, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Identity{}, api.ErrUnknownToken
	}
	if err != nil {
		return api.Identity{}, fmt.Errorf("store: look up token: %w", err)
	}
	id.Scope = api.Scope(scope)
	return id, nil
}

// ActorForToken returns the actor token was issued to; see Identify.
func (p *Postgres) ActorForToken(ctx context.Context, token string) (string, error) {
	id, err := p.Identify(ctx, token)
	return id.Actor, err
}

// RevokeToken revokes token; it reports whether a live token was revoked.
func (p *Postgres) RevokeToken(ctx context.Context, token string) (bool, error) {
	tag, err := p.pool.Exec(ctx, "UPDATE tracker.tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL", HashToken(token))
	if err != nil {
		return false, fmt.Errorf("store: revoke token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RevokeActorTokens revokes every live token of actor and reports how many it
// ended; an expired token is not live, so it is neither ended nor counted. An
// actor with no live token is a success that ended zero: revoking a seat whose
// token is already gone is what a removal path does.
func (p *Postgres) RevokeActorTokens(ctx context.Context, actor string) (int, error) {
	if actor == "" {
		return 0, api.Invalid("a token needs an actor")
	}
	tag, err := p.pool.Exec(ctx, "UPDATE tracker.tokens SET revoked_at = now() WHERE actor = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())", actor)
	if err != nil {
		return 0, fmt.Errorf("store: revoke actor tokens: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RevokeTokenID revokes the live token with id, reporting false when id names
// none that is live (revoking, revoked or expired). The id is compared as
// text, so one that is not a uuid is simply no token, not a failed statement.
func (p *Postgres) RevokeTokenID(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, api.Invalid("revoking by id needs the token id")
	}
	tag, err := p.pool.Exec(ctx, "UPDATE tracker.tokens SET revoked_at = now() WHERE id::text = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())", id)
	if err != nil {
		return false, fmt.Errorf("store: revoke token id: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Ping reports whether the database answers.
func (p *Postgres) Ping(ctx context.Context) error {
	if err := p.pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

// WriteProbe reports whether the database accepts writes: it inserts a row
// into tracker.kv and rolls back, so it fails on a read-only database or a
// standby, and on a schema that was never migrated, and leaves nothing
// behind. (Taking a transaction id is not enough: a read-only transaction
// still gets one.)
func (p *Postgres) WriteProbe(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: write probe: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "INSERT INTO tracker.kv (key, value) VALUES ('_health_probe_' || gen_random_uuid()::text, 'probe')"); err != nil {
		return fmt.Errorf("store: write probe: %w", err)
	}
	return nil
}

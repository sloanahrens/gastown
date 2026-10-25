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

// IssueToken creates a token for actor, valid until expires (never when nil),
// stores its hash, and returns the token. This is the only time the token
// exists in the clear: callers hand it to its owner and never log it.
func (p *Postgres) IssueToken(ctx context.Context, actor string, expires *time.Time) (string, error) {
	if actor == "" {
		return "", api.Invalid("a token needs an actor")
	}
	token, err := generateToken(rand.Reader)
	if err != nil {
		return "", err
	}
	if _, err := p.pool.Exec(ctx, "INSERT INTO tracker.tokens (actor, token_hash, expires_at) VALUES ($1, $2, $3)", actor, HashToken(token), expires); err != nil {
		return "", fmt.Errorf("store: issue token: %w", err)
	}
	return token, nil
}

// ActorForToken returns the actor token was issued to, or api.ErrUnknownToken
// when it is not known, was revoked, or has expired.
func (p *Postgres) ActorForToken(ctx context.Context, token string) (string, error) {
	var actor string
	err := p.pool.QueryRow(ctx,
		"SELECT actor FROM tracker.tokens WHERE token_hash = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())",
		HashToken(token)).Scan(&actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", api.ErrUnknownToken
	}
	if err != nil {
		return "", fmt.Errorf("store: look up token: %w", err)
	}
	return actor, nil
}

// RevokeToken revokes token; it reports whether a live token was revoked.
func (p *Postgres) RevokeToken(ctx context.Context, token string) (bool, error) {
	tag, err := p.pool.Exec(ctx, "UPDATE tracker.tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL", HashToken(token))
	if err != nil {
		return false, fmt.Errorf("store: revoke token: %w", err)
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

// WriteProbe reports whether the database accepts writes: it takes a
// transaction id, which a read-only standby refuses, and rolls back, so it
// leaves nothing behind.
func (p *Postgres) WriteProbe(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: write probe: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var xid string
	if err := tx.QueryRow(ctx, "SELECT pg_current_xact_id()::text").Scan(&xid); err != nil {
		return fmt.Errorf("store: write probe: %w", err)
	}
	return nil
}

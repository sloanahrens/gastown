// Package drivertoken is the admin plane's driver credential (plan row O4,
// gt-uodsl): a token scoped to exactly one workspace, shown once when it is
// issued, and stored only as a SHA-256 hash and an id so no later read of the
// store can present it again.
//
// Nothing calls it yet. Its callers are the console and the driver (later
// beads), which supply a Store over the admin plane's Postgres tables; Memory is
// the in-memory fake those callers and these tests use. The package holds no
// database and no HTTP.
package drivertoken

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/steveyegge/gastown/internal/adminplane/registry"
)

// Record is one issued token as the store keeps it: the id a caller names it by,
// the workspace it is scoped to, its SHA-256 hash, and its lifetime.
type Record struct {
	ID        string
	Workspace string
	Hash      []byte
	IssuedAt  time.Time
	// ExpiresAt is the instant the token stops verifying. The zero value never
	// expires.
	ExpiresAt time.Time
	Revoked   bool
}

// ErrNoStore is a call's refusal of Deps with no Store: nothing could be stored,
// found or revoked.
var ErrNoStore = errors.New("drivertoken: Deps.Store is required")

// ErrNoRandom is Issue's refusal of Deps with no Rand: a token needs bytes no
// caller could predict.
var ErrNoRandom = errors.New("drivertoken: Deps.Rand is required")

// ErrNoClock is a call's refusal of Deps with no Clock: a record stamped with a
// time nobody supplied would read as if it happened when it was stored.
var ErrNoClock = errors.New("drivertoken: Deps.Clock is required")

// ErrNoMayIssue is Issue's refusal of a nil membership check, which would let
// any caller issue a token for any workspace.
var ErrNoMayIssue = errors.New("drivertoken: a membership check is required")

// ErrEmptyWorkspace is Issue's refusal of a workspace with no id, which nothing
// could name.
var ErrEmptyWorkspace = errors.New("drivertoken: a token needs a workspace")

// ErrUnknownToken is Verify's and Lookup's refusal of a token no record holds.
var ErrUnknownToken = errors.New("drivertoken: unknown token")

// ErrRevokedToken is Verify's refusal of a token that was revoked.
var ErrRevokedToken = errors.New("drivertoken: token revoked")

// ErrExpiredToken is Verify's refusal of a token past its ExpiresAt.
var ErrExpiredToken = errors.New("drivertoken: token expired")

// ErrNoRecord is Revoke's refusal of an id that names no token of that
// workspace.
var ErrNoRecord = errors.New("drivertoken: no such token for that workspace")

// Store is where issued tokens live. The admin plane's implementation is its
// Postgres tables (a later bead); Memory is the in-memory fake.
type Store interface {
	// Put stores rec. An error means it was not stored.
	Put(ctx context.Context, rec Record) error
	// Lookup returns the record holding hash, or ErrUnknownToken. An
	// implementation compares the stored hashes to hash in constant time, so
	// the call's duration says nothing about how much of a hash matched;
	// Memory does, and the Postgres table must.
	Lookup(ctx context.Context, hash []byte) (Record, error)
	// Revoke marks the record id, issued for workspace ws, revoked. Naming the
	// workspace keeps a caller from revoking another workspace's token by id.
	Revoke(ctx context.Context, ws, id string) error
}

// Deps is what issuing, verifying and revoking need: where records live, the
// source of token bytes, and the clock a record is stamped with.
type Deps struct {
	Store Store
	// Rand is the source of every token's bytes. It must be a cryptographically
	// secure reader such as crypto/rand.Reader: a caller who can predict its
	// output can predict the tokens issued from it.
	Rand  io.Reader
	Clock registry.Clock
}

// tokenBytes and idBytes are the entropy behind one token and behind its id. The
// id is drawn separately so it never reveals the token's bytes.
const (
	tokenBytes = 32
	idBytes    = 16
)

// Issue returns a token scoped to ws and the Record the store now holds for it,
// and this call is the only one that ever shows the token.
//
// mayIssue is the caller's membership check (plan row O1), supplied here because
// this package cannot see an organization; its refusal is returned unchanged so
// the caller's own typed error reaches the person who asked.
func Issue(ctx context.Context, d Deps, ws string, mayIssue func(context.Context, string) error) (string, Record, error) {
	switch {
	case d.Store == nil:
		return "", Record{}, ErrNoStore
	case d.Rand == nil:
		return "", Record{}, ErrNoRandom
	case d.Clock == nil:
		return "", Record{}, ErrNoClock
	case mayIssue == nil:
		return "", Record{}, ErrNoMayIssue
	case ws == "":
		return "", Record{}, ErrEmptyWorkspace
	}
	if err := mayIssue(ctx, ws); err != nil {
		return "", Record{}, err
	}
	token, err := randomText(d.Rand, tokenBytes)
	if err != nil {
		return "", Record{}, err
	}
	id, err := randomText(d.Rand, idBytes)
	if err != nil {
		return "", Record{}, err
	}
	rec := Record{ID: id, Workspace: ws, Hash: hashOf(token), IssuedAt: d.Clock()}
	if err := d.Store.Put(ctx, rec); err != nil {
		return "", Record{}, fmt.Errorf("drivertoken: storing the issued record: %w", err)
	}
	return token, rec, nil
}

// Verify returns the workspace token was issued for. An unknown, revoked or
// expired token is one of the typed errors and never a workspace.
func Verify(ctx context.Context, d Deps, token string) (string, error) {
	if d.Store == nil {
		return "", ErrNoStore
	}
	if d.Clock == nil {
		return "", ErrNoClock
	}
	if token == "" {
		return "", ErrUnknownToken
	}
	rec, err := d.Store.Lookup(ctx, hashOf(token))
	if err != nil {
		return "", err
	}
	if rec.Revoked {
		return "", ErrRevokedToken
	}
	if !rec.ExpiresAt.IsZero() && !d.Clock().Before(rec.ExpiresAt) {
		return "", ErrExpiredToken
	}
	return rec.Workspace, nil
}

// Revoke marks the token id, issued for ws, revoked, so it no longer verifies.
// Revoking an already revoked token is not an error, so a retried revoke
// succeeds.
func Revoke(ctx context.Context, d Deps, ws, id string) error {
	if d.Store == nil {
		return ErrNoStore
	}
	return d.Store.Revoke(ctx, ws, id)
}

// randomText returns n bytes from r encoded so the value is safe in a URL and in
// a log. A reader that runs dry is an error rather than a shorter token.
func randomText(r io.Reader, n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("drivertoken: reading random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashOf is the only form in which a token reaches a Store.
func hashOf(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Memory is an in-memory Store for tests and for callers before the Postgres
// tables exist. Its zero value is ready to use. It is not safe for concurrent
// use.
type Memory struct {
	records []Record
}

// NewMemory returns an empty Memory.
func NewMemory() *Memory { return &Memory{} }

// Put stores rec.
func (m *Memory) Put(_ context.Context, rec Record) error {
	m.records = append(m.records, rec)
	return nil
}

// Lookup returns the record holding hash, or ErrUnknownToken.
func (m *Memory) Lookup(_ context.Context, hash []byte) (Record, error) {
	for _, rec := range m.records {
		if subtle.ConstantTimeCompare(rec.Hash, hash) == 1 {
			return rec, nil
		}
	}
	return Record{}, ErrUnknownToken
}

// Revoke marks the record id issued for ws revoked, and leaves an already
// revoked record revoked.
func (m *Memory) Revoke(_ context.Context, ws, id string) error {
	for i, rec := range m.records {
		if rec.ID == id && rec.Workspace == ws {
			m.records[i].Revoked = true
			return nil
		}
	}
	return ErrNoRecord
}

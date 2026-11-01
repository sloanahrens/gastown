// Package invite issues and redeems invitations to an organization (plan row
// O3). An invitation is single-use, expires, names one email address, one
// organization and one role, and carries a one-time code that is sent to that
// address. A Google sub is bound as a member only when the verified sign-in
// token reports the invited email as verified and the code matches; the sub,
// not the email, is what the membership keys on (row O1).
//
// Isolation: an invitation belongs to one organization and cannot be replayed
// into a second. A stranger holding the invitation id but not the code, or the
// code but not a verified token for that email, binds nothing, and a code that
// is guessed wrong too often locks the invitation.
//
// The package keeps no secret in a readable form: it stores a hash of the code
// and hands the code to the injected Send function once. It never logs; it has
// no logger. Who may invite is the caller's decision, made before Create.
//
// Nothing calls it yet. Its callers are the console and dashboard handlers (later
// beads), with a verified identity from internal/signin. Storage is a Store
// interface with an in-memory fake; the Postgres tables come later. The package
// does no network I/O.
package invite

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/adminplane/membership"
)

// MaxAttempts is how many wrong codes an invitation survives; the next failure
// and every call after it return ErrLocked.
const MaxAttempts = 5

// Errors; test with errors.Is.
var (
	ErrConfig          = errors.New("invite: service is not configured correctly")
	ErrNotFound        = errors.New("invite: no such invitation")
	ErrUsed            = errors.New("invite: invitation was already used")
	ErrExpired         = errors.New("invite: invitation has expired")
	ErrLocked          = errors.New("invite: too many wrong codes")
	ErrEmailUnverified = errors.New("invite: the sign-in email is not verified")
	ErrEmailMismatch   = errors.New("invite: the sign-in email is not the invited one")
	ErrWrongCode       = errors.New("invite: wrong code")
	ErrBadInvite       = errors.New("invite: invitation needs an organization, an email address and a known role")
	ErrNoSub           = errors.New("invite: a member needs a sub")
)

// Invitation is the stored record. It holds a hash of the code, never the code.
type Invitation struct {
	ID        string
	OrgID     string
	Email     string
	Role      membership.Role
	CodeHash  string
	ExpiresAt time.Time
	Used      bool
	Attempts  int
}

// Store keeps invitations. Consume and Fail must each be atomic, so two
// redemptions of one invitation cannot both succeed.
type Store interface {
	Put(ctx context.Context, inv Invitation) error
	// Get returns ErrNotFound for an unknown id.
	Get(ctx context.Context, id string) (Invitation, error)
	// Consume marks the invitation used, or returns ErrUsed if it already is.
	Consume(ctx context.Context, id string) error
	// Fail counts one wrong code and returns the total so far.
	Fail(ctx context.Context, id string) (int, error)
}

// Sender delivers the one-time code to the invited address. The service calls
// it once per invitation, with the plain code, and keeps no copy.
type Sender func(ctx context.Context, email, invitationID, code string) error

// Config wires a Service.
type Config struct {
	Store   Store
	Members membership.Model
	Send    Sender
	Now     func() time.Time
	Rand    io.Reader
	TTL     time.Duration
}

// Service issues and redeems invitations.
type Service struct{ cfg Config }

// New returns a Service, or ErrConfig.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil, cfg.Members == nil, cfg.Send == nil, cfg.Now == nil, cfg.Rand == nil:
		return nil, fmt.Errorf("%w: store, members, sender, clock and random source are required", ErrConfig)
	case cfg.TTL <= 0:
		return nil, fmt.Errorf("%w: ttl must be positive", ErrConfig)
	}
	return &Service{cfg: cfg}, nil
}

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 32 symbols, no 0/O/1/I

// Create stores an invitation to orgID for email with role and sends the code.
// The organization must exist. It returns the invitation id, which is not a
// secret on its own.
func (s *Service) Create(ctx context.Context, orgID, email string, role membership.Role) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if orgID == "" || !strings.Contains(email, "@") || (role != membership.RoleOwner && role != membership.RoleMember) {
		return "", ErrBadInvite
	}
	if _, err := s.cfg.Members.MembersOf(ctx, orgID); err != nil {
		return "", err
	}
	raw := make([]byte, 24)
	if _, err := io.ReadFull(s.cfg.Rand, raw); err != nil {
		return "", fmt.Errorf("invite: random source: %w", err)
	}
	id := hex.EncodeToString(raw[:16])
	code := make([]byte, 0, 8)
	for _, b := range raw[16:] {
		code = append(code, codeAlphabet[b&31])
	}
	inv := Invitation{
		ID: id, OrgID: orgID, Email: email, Role: role,
		CodeHash:  hashCode(id, string(code)),
		ExpiresAt: s.cfg.Now().Add(s.cfg.TTL),
	}
	if err := s.cfg.Store.Put(ctx, inv); err != nil {
		return "", err
	}
	if err := s.cfg.Send(ctx, email, id, string(code)); err != nil {
		return "", fmt.Errorf("invite: send code: %w", err)
	}
	return id, nil
}

// Bind redeems invitation id for the person whose verified sign-in is sub and
// email. emailVerified is the sign-in token's flag. On success the sub is a
// member of the invitation's organization with its role. An invitation is
// consumed before the member is added, so a failure to add (for example the
// sub is already a member) leaves it used and the owner re-invites.
func (s *Service) Bind(ctx context.Context, id, code, sub, email string, emailVerified bool) (membership.Member, error) {
	inv, err := s.cfg.Store.Get(ctx, id)
	if err != nil {
		return membership.Member{}, err
	}
	if inv.Used {
		return membership.Member{}, ErrUsed
	}
	if !s.cfg.Now().Before(inv.ExpiresAt) {
		return membership.Member{}, ErrExpired
	}
	if inv.Attempts >= MaxAttempts {
		return membership.Member{}, ErrLocked
	}
	if sub == "" {
		return membership.Member{}, ErrNoSub
	}
	if !emailVerified {
		return membership.Member{}, ErrEmailUnverified
	}
	if !strings.EqualFold(strings.TrimSpace(email), inv.Email) {
		return membership.Member{}, ErrEmailMismatch
	}
	want := inv.CodeHash
	got := hashCode(inv.ID, strings.ToUpper(strings.TrimSpace(code)))
	if subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		if _, err := s.cfg.Store.Fail(ctx, id); err != nil {
			return membership.Member{}, err
		}
		return membership.Member{}, ErrWrongCode
	}
	if err := s.cfg.Store.Consume(ctx, id); err != nil {
		return membership.Member{}, err
	}
	m := membership.Member{Sub: sub, Email: inv.Email, Role: inv.Role}
	if err := s.cfg.Members.AddMember(ctx, inv.OrgID, m); err != nil {
		return membership.Member{}, err
	}
	return m, nil
}

// hashCode salts the code with the invitation id so equal codes on two
// invitations hash differently.
func hashCode(id, code string) string {
	sum := sha256.Sum256([]byte(id + ":" + code))
	return hex.EncodeToString(sum[:])
}

// Memory is an in-memory Store for tests and callers before the tables exist.
// It is safe for concurrent use.
type Memory struct {
	mu   sync.Mutex
	byID map[string]Invitation
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty Memory.
func NewMemory() *Memory { return &Memory{byID: map[string]Invitation{}} }

// Put stores inv, replacing any with the same id.
func (m *Memory) Put(_ context.Context, inv Invitation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[inv.ID] = inv
	return nil
}

// Get returns the invitation or ErrNotFound.
func (m *Memory) Get(_ context.Context, id string) (Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.byID[id]
	if !ok {
		return Invitation{}, ErrNotFound
	}
	return inv, nil
}

// Consume marks the invitation used once.
func (m *Memory) Consume(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.byID[id]
	if !ok {
		return ErrNotFound
	}
	if inv.Used {
		return ErrUsed
	}
	inv.Used = true
	m.byID[id] = inv
	return nil
}

// Fail counts a wrong code.
func (m *Memory) Fail(_ context.Context, id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.byID[id]
	if !ok {
		return 0, ErrNotFound
	}
	inv.Attempts++
	m.byID[id] = inv
	return inv.Attempts, nil
}

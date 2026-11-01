// Package signin verifies the ID tokens that identify a person to the hosted
// platform (plan row O2): an Identity Platform token for a customer, and a
// domain-restricted Google token for staff. A verified token yields the Google
// sub, the email and whether Google verified that email. An identity is the sub
// only; the email is carried so an invitation can be matched to it (row O3).
//
// The two kinds are separate types with separate audiences. A staff token never
// passes the customer verifier and a customer token never passes the staff one,
// so neither can stand in for the other at the admin plane or the customer
// dashboard. A failure returns a typed error and a zero Identity, never part of
// one.
//
// The package does no I/O. The signing keys come from a KeyFetcher the caller
// injects, and the clock is injected too. Nothing calls it yet; its callers are
// the console and dashboard handlers (later beads). Tokens are RS256 only: the
// algorithm comes from the verifier, not from the token header.
package signin

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxSkew is the largest clock skew a verifier accepts in its configuration.
const MaxSkew = 5 * time.Minute

// Identity is what a verified token proves. EmailVerified is reported as the
// token states it, never defaulted to true.
type Identity struct {
	Sub           string
	Email         string
	EmailVerified bool
}

// KeyFetcher returns the public key that signs tokens with the given key id.
// The caller owns fetching and caching; an unknown key id is an error.
type KeyFetcher interface {
	Key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// Errors a verifier returns, wrapped with detail; test with errors.Is.
var (
	ErrConfig          = errors.New("signin: verifier is not configured correctly")
	ErrMalformed       = errors.New("signin: token is malformed")
	ErrAlgorithm       = errors.New("signin: token algorithm is not RS256")
	ErrKey             = errors.New("signin: signing key not available")
	ErrSignature       = errors.New("signin: token signature does not verify")
	ErrIssuer          = errors.New("signin: token issuer is not accepted")
	ErrAudience        = errors.New("signin: token audience is not accepted")
	ErrExpired         = errors.New("signin: token has expired")
	ErrNotYetValid     = errors.New("signin: token is not valid yet")
	ErrNoSub           = errors.New("signin: token has no subject")
	ErrDomain          = errors.New("signin: token is not from the staff domain")
	ErrEmailUnverified = errors.New("signin: staff email is not verified")
)

// Config is what both verifiers share.
type Config struct {
	Issuer   string
	Audience string
	Keys     KeyFetcher
	Now      func() time.Time
	// Skew is the clock skew tolerated on exp, nbf and iat; zero means none and
	// anything above MaxSkew is a configuration error.
	Skew time.Duration
}

// Customer verifies customer ID tokens from Identity Platform.
type Customer struct{ cfg Config }

// Staff verifies staff tokens from Google, restricted to one hosted domain.
type Staff struct {
	cfg    Config
	domain string
}

// NewCustomer returns a customer verifier, or ErrConfig.
func NewCustomer(cfg Config) (*Customer, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	return &Customer{cfg: cfg}, nil
}

// NewStaff returns a staff verifier for hostedDomain, or ErrConfig. The domain
// is compared case-insensitively.
func NewStaff(cfg Config, hostedDomain string) (*Staff, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	d := strings.ToLower(strings.TrimSpace(hostedDomain))
	if d == "" || strings.ContainsAny(d, "@ ") {
		return nil, fmt.Errorf("%w: staff domain %q", ErrConfig, hostedDomain)
	}
	return &Staff{cfg: cfg, domain: d}, nil
}

func (c Config) check() error {
	switch {
	case c.Issuer == "", c.Audience == "":
		return fmt.Errorf("%w: issuer and audience are required", ErrConfig)
	case c.Keys == nil:
		return fmt.Errorf("%w: a key fetcher is required", ErrConfig)
	case c.Now == nil:
		return fmt.Errorf("%w: a clock is required", ErrConfig)
	case c.Skew < 0 || c.Skew > MaxSkew:
		return fmt.Errorf("%w: skew %v outside 0 to %v", ErrConfig, c.Skew, MaxSkew)
	}
	return nil
}

// Verify checks a customer token and returns the identity it proves.
func (v *Customer) Verify(ctx context.Context, token string) (Identity, error) {
	cl, err := verify(ctx, v.cfg, token)
	if err != nil {
		return Identity{}, err
	}
	return cl.identity(), nil
}

// Verify checks a staff token: the usual checks, then a verified email and a
// hosted-domain claim and email address that both belong to the staff domain.
func (v *Staff) Verify(ctx context.Context, token string) (Identity, error) {
	cl, err := verify(ctx, v.cfg, token)
	if err != nil {
		return Identity{}, err
	}
	if strings.ToLower(cl.HD) != v.domain {
		return Identity{}, fmt.Errorf("%w: hd claim %q", ErrDomain, cl.HD)
	}
	if !strings.HasSuffix(strings.ToLower(cl.Email), "@"+v.domain) {
		return Identity{}, fmt.Errorf("%w: email is outside the domain", ErrDomain)
	}
	if !cl.EmailVerified {
		return Identity{}, ErrEmailUnverified
	}
	return cl.identity(), nil
}

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type claims struct {
	Iss           string          `json:"iss"`
	Aud           json.RawMessage `json:"aud"`
	Sub           string          `json:"sub"`
	Exp           *int64          `json:"exp"`
	Nbf           *int64          `json:"nbf"`
	Iat           *int64          `json:"iat"`
	Email         string          `json:"email"`
	EmailVerified bool            `json:"email_verified"`
	HD            string          `json:"hd"`
}

func (c claims) identity() Identity {
	return Identity{Sub: c.Sub, Email: c.Email, EmailVerified: c.EmailVerified}
}

func verify(ctx context.Context, cfg Config, token string) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims{}, fmt.Errorf("%w: want 3 parts, have %d", ErrMalformed, len(parts))
	}
	var h header
	if err := decodePart(parts[0], &h); err != nil {
		return claims{}, fmt.Errorf("%w: header: %v", ErrMalformed, err)
	}
	if h.Alg != "RS256" {
		return claims{}, fmt.Errorf("%w: %q", ErrAlgorithm, h.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims{}, fmt.Errorf("%w: signature encoding", ErrMalformed)
	}
	key, err := cfg.Keys.Key(ctx, h.Kid)
	if err != nil || key == nil {
		return claims{}, fmt.Errorf("%w: kid %q: %v", ErrKey, h.Kid, err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return claims{}, ErrSignature
	}
	var cl claims
	if err := decodePart(parts[1], &cl); err != nil {
		return claims{}, fmt.Errorf("%w: claims: %v", ErrMalformed, err)
	}
	if err := cl.check(cfg); err != nil {
		return claims{}, err
	}
	return cl, nil
}

func (cl claims) check(cfg Config) error {
	if cl.Iss != cfg.Issuer {
		return fmt.Errorf("%w: %q", ErrIssuer, cl.Iss)
	}
	if !audienceHas(cl.Aud, cfg.Audience) {
		return ErrAudience
	}
	if cl.Sub == "" {
		return ErrNoSub
	}
	if cl.Exp == nil {
		return fmt.Errorf("%w: no exp claim", ErrMalformed)
	}
	now := cfg.Now()
	if !now.Before(time.Unix(*cl.Exp, 0).Add(cfg.Skew)) {
		return ErrExpired
	}
	for _, t := range []*int64{cl.Nbf, cl.Iat} {
		if t != nil && now.Add(cfg.Skew).Before(time.Unix(*t, 0)) {
			return ErrNotYetValid
		}
	}
	return nil
}

// audienceHas reports whether the aud claim, a string or a list of strings,
// holds want exactly.
func audienceHas(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

func decodePart(part string, into any) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

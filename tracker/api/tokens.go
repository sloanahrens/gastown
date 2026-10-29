package api

import (
	"strings"
	"unicode"
)

// Tokens are issued and revoked over the admin routes, so a caller that holds
// an admin token can hand a seat its own without ever holding the database
// password. An issued token is returned once, in the answer that issues it,
// and never logged: what the store keeps is its hash.

// MaxTokenActorLen bounds the actor a token is issued to or revoked for. A
// seat address is short, so a longer string is a caller's mistake, not a seat.
const MaxTokenActorLen = 128

// IssueTokenRequest asks for a new token for Actor with Scope; an empty Scope
// asks for a seat's token.
type IssueTokenRequest struct {
	Actor string `json:"actor"`
	Scope Scope  `json:"scope,omitempty"`
}

// ScopeOrSeat is the scope the request asks for: its own, or seat when it
// names none.
func (r IssueTokenRequest) ScopeOrSeat() Scope {
	if r.Scope == "" {
		return ScopeSeat
	}
	return r.Scope
}

// Validate refuses a request that names no usable actor or a scope no token
// can have.
func (r IssueTokenRequest) Validate() error {
	if err := validateTokenActor(r.Actor); err != nil {
		return err
	}
	if !r.ScopeOrSeat().Valid() {
		return Invalid("scope %q is not seat or admin", r.Scope)
	}
	return nil
}

// IssuedToken is the one answer that carries a token: no read of the tracker
// returns it again.
type IssuedToken struct {
	Actor string `json:"actor"`
	Scope Scope  `json:"scope"`
	Token string `json:"token"`
}

// RevokeTokensRequest names what a revoke ends: every live token of Actor, or
// the one live token whose id is TokenID; exactly one of them is set.
type RevokeTokensRequest struct {
	Actor   string `json:"actor,omitempty"`
	TokenID string `json:"token_id,omitempty"`
}

// Validate refuses a request that names both or neither, and an actor no
// token could have been issued to.
func (r RevokeTokensRequest) Validate() error {
	switch {
	case r.Actor != "" && r.TokenID != "":
		return Invalid("revoke names an actor or a token_id, not both")
	case r.Actor != "":
		return validateTokenActor(r.Actor)
	case r.TokenID == "":
		return Invalid("revoke needs an actor or a token_id")
	}
	return nil
}

// RevokeTokensResult counts the live tokens a revoke ended; zero is a success
// for an actor that had none.
type RevokeTokensResult struct {
	Revoked int `json:"revoked"`
}

// validateTokenActor refuses an actor no token may be issued to: blank or
// padded, over MaxTokenActorLen, or holding whitespace or a control character
// (so not one address).
func validateTokenActor(actor string) error {
	switch {
	case strings.TrimSpace(actor) == "":
		return Invalid("a token needs an actor")
	case len(actor) > MaxTokenActorLen:
		return Invalid("actor is %d bytes, at most %d", len(actor), MaxTokenActorLen)
	case strings.ContainsFunc(actor, unicode.IsSpace):
		return Invalid("actor %q holds whitespace; a seat address has none", actor)
	case strings.ContainsFunc(actor, unicode.IsControl):
		return Invalid("actor %q holds a control character; a seat address has none", actor)
	}
	return nil
}

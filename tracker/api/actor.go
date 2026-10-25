package api

import "context"

type actorKey struct{}

// WithActor returns ctx carrying the actor a request acts for. The server
// puts the token's actor here; store writes that take no actor argument
// (close, delete) record it on their events.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom is the actor WithActor stored, or "".
func ActorFrom(ctx context.Context) string {
	a, _ := ctx.Value(actorKey{}).(string)
	return a
}

// Scope is what a token may do.
type Scope string

const (
	// ScopeSeat may use every issue verb.
	ScopeSeat Scope = "seat"
	// ScopeAdmin may also write configuration, import and export, issue tokens
	// and migrate. The daemon and operators hold it; a seat never does.
	ScopeAdmin Scope = "admin"
)

// Valid reports whether s is a scope a token can have.
func (s Scope) Valid() bool { return s == ScopeSeat || s == ScopeAdmin }

// Identity is who a token belongs to and what it may do.
type Identity struct {
	Actor string
	Scope Scope
}

type scopeKey struct{}

// WithIdentity returns ctx carrying the identity of an authenticated request:
// its actor (read with ActorFrom) and its scope (read with ScopeFrom).
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(WithActor(ctx, id.Actor), scopeKey{}, id.Scope)
}

// ScopeFrom is the scope WithIdentity stored, or "" for a context that holds
// none, which no scope check admits.
func ScopeFrom(ctx context.Context) Scope {
	s, _ := ctx.Value(scopeKey{}).(Scope)
	return s
}

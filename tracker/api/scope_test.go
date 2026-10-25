package api

import (
	"context"
	"errors"
	"testing"
)

func TestScopeValidity(t *testing.T) {
	t.Parallel() // unit tier: pure
	for scope, want := range map[Scope]bool{ScopeSeat: true, ScopeAdmin: true, "": false, "root": false, "Admin": false} {
		if got := scope.Valid(); got != want {
			t.Errorf("Scope(%q).Valid() = %v, want %v", scope, got, want)
		}
	}
}

func TestIdentityTravelsOnTheContextAndAnEmptyContextHasNoScope(t *testing.T) {
	t.Parallel()
	ctx := WithIdentity(context.Background(), Identity{Actor: "town/daemon", Scope: ScopeAdmin})
	if ActorFrom(ctx) != "town/daemon" || ScopeFrom(ctx) != ScopeAdmin {
		t.Errorf("identity = %q / %q, want town/daemon / admin", ActorFrom(ctx), ScopeFrom(ctx))
	}
	if ScopeFrom(context.Background()) != "" {
		t.Error("a context without an identity must have no scope, so no scope check admits it")
	}
}

func TestForbiddenIsA403AndMatchesItsSentinel(t *testing.T) {
	t.Parallel()
	e := Forbidden("this needs an admin token")
	if e.HTTPStatus() != 403 || !errors.Is(e, ErrForbidden) || errors.Is(e, ErrInvalid) {
		t.Errorf("Forbidden = status %d, is ErrForbidden %v; want 403 and the forbidden sentinel only", e.HTTPStatus(), errors.Is(e, ErrForbidden))
	}
}

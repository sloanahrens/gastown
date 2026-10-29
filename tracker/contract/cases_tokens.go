package contract

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "a token resolves to the actor and scope it was issued for, and an unknown one resolves to nobody", Fake: true, Run: tokensResolve})
	register(Case{Name: "issuing is refused without an actor or with a scope no token has, and two issues make two tokens", Fake: true, Run: tokensIssueRefusals})
	register(Case{Name: "revoking an actor ends every live token of it and leaves every other actor's", Fake: true, Run: tokensRevokeActor})
	register(Case{Name: "revoking an actor with no live token succeeds and ends none", Fake: true, Run: tokensRevokeNobody})
	register(Case{Name: "an expired token resolves to nobody and is not counted as revoked", Fake: true, Run: tokensExpired})
}

func tokensResolve(t *testing.T, s store.Store) {
	token, err := s.IssueTokenScoped(ctx(), "gastown/polecats/bullet", api.ScopeSeat, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Identify(ctx(), token)
	if err != nil || id.Actor != "gastown/polecats/bullet" || id.Scope != api.ScopeSeat {
		t.Errorf("Identify = %+v, %v; want the seat its token was issued for", id, err)
	}
	admin, err := s.IssueTokenScoped(ctx(), "town/daemon", api.ScopeAdmin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := s.Identify(ctx(), admin); err != nil || id.Actor != "town/daemon" || id.Scope != api.ScopeAdmin {
		t.Errorf("Identify of an admin token = %+v, %v; want the admin scope", id, err)
	}
	if _, err := s.Identify(ctx(), "trk_not-a-token"); !errors.Is(err, api.ErrUnknownToken) {
		t.Errorf("Identify of an unknown token = %v, want api.ErrUnknownToken", err)
	}
}

func tokensIssueRefusals(t *testing.T, s store.Store) {
	if _, err := s.IssueTokenScoped(ctx(), "", api.ScopeSeat, nil); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("issuing for no actor = %v, want api.ErrInvalid", err)
	}
	if _, err := s.IssueTokenScoped(ctx(), "a", "root", nil); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("issuing with scope root = %v, want api.ErrInvalid", err)
	}
	// Nothing is idempotent here: two issues for one actor make two tokens,
	// and revoking one of them leaves the other live.
	first, err := s.IssueTokenScoped(ctx(), "gastown/polecats/bullet", api.ScopeSeat, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.IssueTokenScoped(ctx(), "gastown/polecats/bullet", api.ScopeSeat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two issues for one actor returned the same token")
	}
	for _, token := range []string{first, second} {
		if _, err := s.Identify(ctx(), token); err != nil {
			t.Errorf("Identify of a token issued beside another = %v, want it to resolve", err)
		}
	}
}

func tokensRevokeActor(t *testing.T, s store.Store) {
	first, err := s.IssueTokenScoped(ctx(), "gastown/polecats/bullet", api.ScopeSeat, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.IssueTokenScoped(ctx(), "gastown/polecats/bullet", api.ScopeSeat, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.IssueTokenScoped(ctx(), "gastown/polecats/angharad", api.ScopeSeat, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.RevokeActorTokens(ctx(), "gastown/polecats/bullet")
	if err != nil || n != 2 {
		t.Fatalf("RevokeActorTokens = %d, %v; want 2", n, err)
	}
	for name, token := range map[string]string{"first": first, "second": second} {
		if _, err := s.Identify(ctx(), token); !errors.Is(err, api.ErrUnknownToken) {
			t.Errorf("the %s token of a revoked actor = %v, want api.ErrUnknownToken", name, err)
		}
	}
	if id, err := s.Identify(ctx(), other); err != nil || id.Actor != "gastown/polecats/angharad" {
		t.Errorf("another actor's token after the revoke = %+v, %v; want it still live", id, err)
	}
	// A second revoke of the same actor ends nothing: the tokens are gone.
	if n, err := s.RevokeActorTokens(ctx(), "gastown/polecats/bullet"); err != nil || n != 0 {
		t.Errorf("revoking an actor with no live token = %d, %v; want 0 and no error", n, err)
	}
}

func tokensRevokeNobody(t *testing.T, s store.Store) {
	if n, err := s.RevokeActorTokens(ctx(), "gastown/polecats/nobody"); err != nil || n != 0 {
		t.Errorf("RevokeActorTokens of an unknown actor = %d, %v; want 0 and no error", n, err)
	}
	if n, err := s.RevokeActorTokens(ctx(), ""); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("RevokeActorTokens with no actor = %d, %v; want api.ErrInvalid", n, err)
	}
}

func tokensExpired(t *testing.T, s store.Store) {
	// "Live" means usable: an expired token resolves to nobody, and revoking
	// its actor counts only the live one, so the count is not a token that was
	// already unusable.
	past := time.Now().Add(-time.Hour)
	expired, err := s.IssueTokenScoped(ctx(), "gastown/polecats/bullet", api.ScopeSeat, &past)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Identify(ctx(), expired); !errors.Is(err, api.ErrUnknownToken) {
		t.Errorf("an expired token = %v, want api.ErrUnknownToken", err)
	}
	live, err := s.IssueTokenScoped(ctx(), "gastown/polecats/bullet", api.ScopeSeat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.RevokeActorTokens(ctx(), "gastown/polecats/bullet"); err != nil || n != 1 {
		t.Errorf("revoking an actor with one live and one expired token = %d, %v; want 1", n, err)
	}
	if _, err := s.Identify(ctx(), live); !errors.Is(err, api.ErrUnknownToken) {
		t.Errorf("the live token after its actor was revoked = %v, want api.ErrUnknownToken", err)
	}
}

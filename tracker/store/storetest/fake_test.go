package storetest

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/contract"
	"github.com/steveyegge/gastown/tracker/internal/leakcheck"
	"github.com/steveyegge/gastown/tracker/store"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Main(m)) }

// The fake is held to the same contract as Postgres. Unit tier: in-memory,
// microseconds per case.
func TestFakeSatisfiesItsPartOfTheStoreContract(t *testing.T) {
	contract.RunFakeContract(t, func(*testing.T) store.Store { return NewFake() })
}

// A token is issued and revoked for an actor the token routes accept, so the
// fake refuses the actors they refuse: a unit test cannot mint or end a token
// for an identity the API never names.
func TestTheFakeRefusesTokenActorsTheRoutesRefuse(t *testing.T) {
	t.Parallel()
	for name, actor := range map[string]string{
		"a padded actor":      "  gastown/polecats/bullet  ",
		"whitespace inside":   "two seats",
		"a control character": "seat\x1bone",
		"an overlong actor":   strings.Repeat("a", api.MaxTokenActorLen+1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := (api.IssueTokenRequest{Actor: actor}).Validate(); err == nil {
				t.Fatalf("the routes accept %q, so this case names no refusal", actor)
			}
			f := NewFake()
			if _, err := f.IssueTokenScoped(t.Context(), actor, api.ScopeSeat, nil); !errors.Is(err, api.ErrInvalid) {
				t.Errorf("IssueTokenScoped(%q) = %v, want api.ErrInvalid", actor, err)
			}
			if n, err := f.RevokeActorTokens(t.Context(), actor); !errors.Is(err, api.ErrInvalid) {
				t.Errorf("RevokeActorTokens(%q) = %d, %v; want api.ErrInvalid", actor, n, err)
			}
		})
	}
}

// A refused revoke ends nothing: the token of the actor it almost names, one
// space away, stays live.
func TestARefusedRevokeEndsNoToken(t *testing.T) {
	t.Parallel()
	f := NewFake()
	token, err := f.IssueTokenScoped(t.Context(), "gastown/polecats/bullet", api.ScopeSeat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.RevokeActorTokens(t.Context(), "gastown/polecats/bullet "); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("RevokeActorTokens of a padded actor = %v, want api.ErrInvalid", err)
	}
	if _, err := f.Identify(t.Context(), token); err != nil {
		t.Errorf("a token after a refused revoke of its near actor = %v, want it still live", err)
	}
}

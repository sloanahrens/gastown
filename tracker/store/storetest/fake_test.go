package storetest

import (
	"errors"
	"os"
	"slices"
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

// Stats counts a rig's durable issues by status and its wisps apart, so a
// wisp is never in the durable total or in ByStatus.
func TestTheFakeStatsCountsWispsApartFromDurableIssues(t *testing.T) {
	t.Parallel()
	f := NewFake()
	ctx := t.Context()
	if _, err := f.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "open"}); err != nil {
		t.Fatal(err)
	}
	done, err := f.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.CloseIssues(ctx, []string{done.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "a wisp", Ephemeral: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateIssue(ctx, api.CreateParams{Rig: "hq", Title: "other rig"}); err != nil {
		t.Fatal(err)
	}

	gt, err := f.Stats(ctx, "gt")
	if err != nil {
		t.Fatal(err)
	}
	if gt.Total != 2 || gt.ByStatus["open"] != 1 || gt.ByStatus["closed"] != 1 || gt.Wisps != 1 {
		t.Errorf("gt stats = %+v, want 2 durable (one open, one closed) and 1 wisp", gt)
	}
	all, err := f.Stats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if all.Total != 3 || all.Wisps != 1 {
		t.Errorf("stats of every rig = %+v, want 3 durable and 1 wisp", all)
	}
	empty, err := f.Stats(ctx, "nosuchrig")
	if err != nil {
		t.Fatal(err)
	}
	if empty.Total != 0 || empty.Wisps != 0 || empty.ByStatus == nil {
		t.Errorf("stats of a rig with nothing = %+v, want zero and a non-nil ByStatus", empty)
	}
}

// The external-target reverse read follows the rule in the contract case
// depsExternalDependents: a dependent is listed under the target it names,
// once, at the plain relation when it also links plainly.
func TestFakeDependentsOfExternalTargets(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFake()
	for _, id := range []string{"gt-2", "gt-12", "gt-3", "gt-4"} {
		if _, err := f.CreateIssue(ctx, api.CreateParams{ID: id, Title: id}); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []struct{ issue, on, typ string }{
		{"gt-3", "external:om:gt-2", "blocks"},
		{"gt-3", "gt-2", "tracks"},
		{"gt-4", "external:om:gt-12", "blocks"},
	} {
		if err := f.AddDependency(ctx, l.issue, l.on, l.typ); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.GetIssues(ctx, []string{"gt-2", "gt-12"})
	if err != nil {
		t.Fatal(err)
	}
	dependents := func(id string) []string {
		var rows []string
		for _, d := range got[id].Dependents {
			rows = append(rows, d.ID+" "+d.DependencyType)
		}
		return rows
	}
	if want := []string{"gt-3 tracks"}; !slices.Equal(dependents("gt-2"), want) {
		t.Errorf("Dependents of gt-2 = %v, want %v: gt-3 once, at its plain relation; gt-4 names gt-12, not gt-2", dependents("gt-2"), want)
	}
	if want := []string{"gt-4 blocks"}; !slices.Equal(dependents("gt-12"), want) {
		t.Errorf("Dependents of gt-12 = %v, want %v", dependents("gt-12"), want)
	}
}

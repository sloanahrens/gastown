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

// closeFenceCase seeds one close and says what the fake must answer and hold
// afterwards. seed returns the ids the close names.
type closeFenceCase struct {
	name    string
	seed    func(t *testing.T, f *Fake) []string
	actor   string
	force   bool
	wantErr error
	want    map[string]string // id -> status after the call
}

// TestFakeCloseIssuesRefusesEveryFenceThePostgresCloseVerbRefuses pins every
// fence Fake.CloseIssues applies, as closeRow(fromClose=true) does: a pin,
// another actor's claim, an open child and an open blocker. The fake took these
// on as a global behaviour change for the bd shim's close answer (gt-p85pw.96),
// so each row holds the statuses after the call and a refusal that closed half
// a batch is red as well as one that closed what the real store refuses.
func TestFakeCloseIssuesRefusesEveryFenceThePostgresCloseVerbRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []closeFenceCase{
		{
			name: "a pinned issue is refused",
			seed: func(t *testing.T, f *Fake) []string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "pinned"})
				mustFakeStatus(t, f, "gt-1", "pinned")
				return []string{"gt-1"}
			},
			wantErr: api.ErrConflict,
			want:    map[string]string{"gt-1": "pinned"},
		},
		{
			name: "another actor's claim is refused",
			seed: func(t *testing.T, f *Fake) []string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "held", Assignee: "alice"})
				return []string{"gt-1"}
			},
			actor:   "bob",
			wantErr: api.ErrConflict,
			want:    map[string]string{"gt-1": "open"},
		},
		{
			name: "an open child is refused",
			seed: func(t *testing.T, f *Fake) []string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "epic"})
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-2", Title: "kid", Parent: "gt-1"})
				return []string{"gt-1"}
			},
			wantErr: api.ErrConflict,
			want:    map[string]string{"gt-1": "open", "gt-2": "open"},
		},
		{
			name: "an open blocker is refused",
			seed: func(t *testing.T, f *Fake) []string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "work"})
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-2", Title: "blocker"})
				mustFakeDep(t, f, "gt-1", "gt-2", "blocks")
				return []string{"gt-1"}
			},
			wantErr: api.ErrConflict,
			want:    map[string]string{"gt-1": "open"},
		},
		{
			name: "the holder's own claim is not a fence",
			seed: func(t *testing.T, f *Fake) []string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "held", Assignee: "alice"})
				return []string{"gt-1"}
			},
			actor: "alice",
			want:  map[string]string{"gt-1": "closed"},
		},
		{
			name: "a child the same call closes does not fence it",
			seed: func(t *testing.T, f *Fake) []string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "epic"})
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-2", Title: "kid", Parent: "gt-1"})
				return []string{"gt-1", "gt-2"}
			},
			want: map[string]string{"gt-1": "closed", "gt-2": "closed"},
		},
		{
			name: "force closes through a pin and an open child",
			seed: func(t *testing.T, f *Fake) []string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "epic"})
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-2", Title: "kid", Parent: "gt-1"})
				mustFakeStatus(t, f, "gt-1", "pinned")
				return []string{"gt-1"}
			},
			force: true,
			want:  map[string]string{"gt-1": "closed", "gt-2": "open"},
		},
		{
			name: "an already closed issue is skipped, not refused",
			seed: func(t *testing.T, f *Fake) []string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "done", Assignee: "alice"})
				if err := f.CloseIssues(api.WithActor(t.Context(), "alice"), []string{"gt-1"}, "r", false); err != nil {
					t.Fatalf("the first close: %v", err)
				}
				return []string{"gt-1"}
			},
			actor: "bob",
			want:  map[string]string{"gt-1": "closed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFake()
			ids := tc.seed(t, f)
			err := f.CloseIssues(api.WithActor(t.Context(), tc.actor), ids, "r", tc.force)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("CloseIssues(%v) = %v, want %v", ids, err, tc.wantErr)
			}
			for id, want := range tc.want {
				if got := mustFakeGet(t, f, id).Status; got != want {
					t.Errorf("%s after CloseIssues(%v) = %s, want %s", id, ids, got, want)
				}
			}
		})
	}
}

// TestFakeStatusWriteObeysOnlyTheFencesPostgresAppliesToIt pins the narrower
// half of the same behaviour: the status write is not the close verb, so
// Postgres runs closeRow with fromClose=false there (postgres_update.go) and a
// pin or another actor's claim does not refuse it. The open child and blocker
// still do, and a refused write leaves the issue as it was.
func TestFakeStatusWriteObeysOnlyTheFencesPostgresAppliesToIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		seed    func(t *testing.T, f *Fake) string
		actor   string
		force   bool
		wantErr error
		want    map[string]string // id -> status after the call
	}{
		{
			name: "a pinned issue closes",
			seed: func(t *testing.T, f *Fake) string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "pinned"})
				mustFakeStatus(t, f, "gt-1", "pinned")
				return "gt-1"
			},
			want: map[string]string{"gt-1": "closed"},
		},
		{
			name: "another actor's claim does not fence it",
			seed: func(t *testing.T, f *Fake) string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "held", Assignee: "alice"})
				return "gt-1"
			},
			actor: "bob",
			want:  map[string]string{"gt-1": "closed"},
		},
		{
			name: "an open child is refused",
			seed: func(t *testing.T, f *Fake) string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "epic"})
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-2", Title: "kid", Parent: "gt-1"})
				return "gt-1"
			},
			wantErr: api.ErrConflict,
			want:    map[string]string{"gt-1": "open", "gt-2": "open"},
		},
		{
			name: "an open blocker is refused",
			seed: func(t *testing.T, f *Fake) string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "work"})
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-2", Title: "blocker"})
				mustFakeDep(t, f, "gt-1", "gt-2", "blocks")
				return "gt-1"
			},
			wantErr: api.ErrConflict,
			want:    map[string]string{"gt-1": "open"},
		},
		{
			name: "force closes through an open child",
			seed: func(t *testing.T, f *Fake) string {
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-1", Title: "epic"})
				mustFakeCreate(t, f, api.CreateParams{ID: "gt-2", Title: "kid", Parent: "gt-1"})
				return "gt-1"
			},
			force: true,
			want:  map[string]string{"gt-1": "closed", "gt-2": "open"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFake()
			id := tc.seed(t, f)
			closed := "closed"
			err := f.UpdateIssue(t.Context(), id, api.UpdatePatch{Status: &closed, Force: tc.force, Actor: tc.actor}, api.Guard{})
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("UpdateIssue(%s, status closed) = %v, want %v", id, err, tc.wantErr)
			}
			for wantID, want := range tc.want {
				got := mustFakeGet(t, f, wantID)
				if got.Status != want {
					t.Errorf("%s after the status write = %s, want %s", wantID, got.Status, want)
				}
				if want == "closed" {
					if got.ClosedAt == nil || got.CloseReason != "" {
						t.Errorf("%s after a closed status write: ClosedAt = %v, CloseReason = %q, want the close time and no reason", wantID, got.ClosedAt, got.CloseReason)
					}
					continue
				}
				if got.ClosedAt != nil {
					t.Errorf("%s after a refused status write has ClosedAt = %v, want the issue untouched", wantID, *got.ClosedAt)
				}
			}
		})
	}
}

// Tests use these four helpers to lay a state down without asserting on the
// setup itself; a setup the fake refuses fails the test at once.
func mustFakeCreate(t *testing.T, f *Fake, p api.CreateParams) {
	t.Helper()
	if _, err := f.CreateIssue(t.Context(), p); err != nil {
		t.Fatalf("CreateIssue(%s): %v", p.ID, err)
	}
}

func mustFakeStatus(t *testing.T, f *Fake, id, status string) {
	t.Helper()
	if err := f.UpdateIssue(t.Context(), id, api.UpdatePatch{Status: &status}, api.Guard{}); err != nil {
		t.Fatalf("UpdateIssue(%s, status %s): %v", id, status, err)
	}
}

func mustFakeDep(t *testing.T, f *Fake, issue, dependsOn, depType string) {
	t.Helper()
	if err := f.AddDependency(t.Context(), issue, dependsOn, depType); err != nil {
		t.Fatalf("AddDependency(%s -> %s): %v", issue, dependsOn, err)
	}
}

func mustFakeGet(t *testing.T, f *Fake, id string) api.Issue {
	t.Helper()
	got, err := f.GetIssues(t.Context(), []string{id})
	if err != nil {
		t.Fatalf("GetIssues(%s): %v", id, err)
	}
	is, ok := got[id]
	if !ok {
		t.Fatalf("%s is missing after the write", id)
	}
	return is
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

// The delete the bd shim answers is the store's: all-or-nothing, links gone in
// both directions, and a surviving child left without a parent.
func TestTheFakeDeletesABatchAllOrNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFake()
	parent, err := f.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	kid, err := f.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "kid", Parent: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.AddDependency(ctx, other.ID, parent.ID, "blocks"); err != nil {
		t.Fatal(err)
	}

	if err := f.DeleteIssues(ctx, []string{parent.ID, "gt-missing"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("delete with a missing id = %v, want api.ErrNotFound", err)
	}
	if got, err := f.GetIssues(ctx, []string{parent.ID, other.ID}); err != nil || len(got) != 2 {
		t.Fatalf("after the refused batch: %v (%v), want both issues still there", got, err)
	}

	if err := f.DeleteIssues(ctx, []string{parent.ID}); err != nil {
		t.Fatal(err)
	}
	if gone, err := f.GetIssues(ctx, []string{parent.ID}); err != nil || len(gone) != 0 {
		t.Fatalf("the deleted parent still exists: %v (%v)", gone, err)
	}
	got, err := f.GetIssues(ctx, []string{kid.ID, other.ID})
	if err != nil {
		t.Fatal(err)
	}
	if k, ok := got[kid.ID]; !ok || k.Parent != "" {
		t.Errorf("child after its parent was deleted = %+v (exists %v), want it kept with no parent", k, ok)
	}
	var links []string
	if err := f.Export(ctx, "gt", func(r api.ExportRecord) error {
		if r.Dependency != nil {
			links = append(links, r.Dependency.IssueID+" -> "+r.Dependency.DependsOn)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(links, other.ID+" -> "+parent.ID) {
		t.Errorf("the links of a deleted issue survive: %v", links)
	}
}

// The fake writes the text fields the bd shim's delete and defer handlers
// write, and still refuses a patch field it does not model rather than applying
// the rest of the patch.
func TestTheFakeWritesTheTextFieldsTheShimUpdates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFake()
	is, err := f.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "before", Description: "before"})
	if err != nil {
		t.Fatal(err)
	}
	title, desc, acceptance := "after", "after [deleted:gt-2]", "- [ ] done"
	if err := f.UpdateIssue(ctx, is.ID, api.UpdatePatch{
		Title: &title, Description: &desc, Acceptance: &acceptance, AppendNotes: "first",
	}, api.Guard{}); err != nil {
		t.Fatal(err)
	}
	if err := f.UpdateIssue(ctx, is.ID, api.UpdatePatch{AppendNotes: "second"}, api.Guard{}); err != nil {
		t.Fatal(err)
	}
	got, err := f.GetIssues(ctx, []string{is.ID})
	if err != nil {
		t.Fatal(err)
	}
	after := got[is.ID]
	if after.Title != title || after.Description != desc || after.AcceptanceCriteria != acceptance {
		t.Errorf("after the text write: %+v, want %q, %q and %q", after, title, desc, acceptance)
	}
	if want := "first\nsecond"; after.Notes != want {
		t.Errorf("notes = %q, want %q", after.Notes, want)
	}

	priority := 1
	if err := f.UpdateIssue(ctx, is.ID, api.UpdatePatch{Priority: &priority, Description: &title}, api.Guard{}); !errors.Is(err, store.ErrUnimplemented) {
		t.Fatalf("a patch with an unmodeled field = %v, want store.ErrUnimplemented", err)
	}
	refused, err := f.GetIssues(ctx, []string{is.ID})
	if err != nil {
		t.Fatal(err)
	}
	if refused[is.ID].Description != desc {
		t.Errorf("the refused patch wrote its modeled field: %q", refused[is.ID].Description)
	}
}

// The fake applies the label fields of a patch the bd shim's label add handler
// writes, in the Postgres store's order, and a patch with no change leaves the
// issue's labels as they were.
func TestTheFakeAppliesTheLabelPatchesTheShimWrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFake()
	is, err := f.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "t", Labels: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	labels := func() []string {
		t.Helper()
		got, err := f.GetIssues(ctx, []string{is.ID})
		if err != nil {
			t.Fatal(err)
		}
		return got[is.ID].Labels
	}
	if err := f.UpdateIssue(ctx, is.ID, api.UpdatePatch{AddLabels: []string{"c", "a"}}, api.Guard{}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if got, want := labels(), []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("after AddLabels labels = %v, want %v", got, want)
	}
	if err := f.UpdateIssue(ctx, is.ID, api.UpdatePatch{RemoveLabels: []string{"a"}}, api.Guard{}); err != nil {
		t.Fatalf("RemoveLabels: %v", err)
	}
	if got, want := labels(), []string{"b", "c"}; !slices.Equal(got, want) {
		t.Errorf("after RemoveLabels labels = %v, want %v", got, want)
	}
	if err := f.UpdateIssue(ctx, is.ID, api.UpdatePatch{SetLabels: []string{"z", "z"}}, api.Guard{}); err != nil {
		t.Fatalf("SetLabels: %v", err)
	}
	if got, want := labels(), []string{"z"}; !slices.Equal(got, want) {
		t.Errorf("after SetLabels labels = %v, want %v", got, want)
	}
	// A patch that changes nothing writes nothing, as Postgres leaves the row
	// alone; a refused field still refuses the whole patch.
	before := mustFakeIssue(t, f, is.ID)
	if err := f.UpdateIssue(ctx, is.ID, api.UpdatePatch{AddLabels: []string{"z"}}, api.Guard{}); err != nil {
		t.Fatalf("adding a label the issue already carries: %v", err)
	}
	if after := mustFakeIssue(t, f, is.ID); !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("a label patch that changed nothing moved UpdatedAt from %v to %v", before.UpdatedAt, after.UpdatedAt)
	}
	priority := 1
	if err := f.UpdateIssue(ctx, is.ID, api.UpdatePatch{Priority: &priority, AddLabels: []string{"y"}}, api.Guard{}); !errors.Is(err, store.ErrUnimplemented) {
		t.Fatalf("a patch with an unmodeled field = %v, want store.ErrUnimplemented", err)
	}
	if got, want := labels(), []string{"z"}; !slices.Equal(got, want) {
		t.Errorf("the refused patch wrote its labels: %v, want %v", got, want)
	}
}

// SeedComment keeps the id the fixture names, so a replay reads the comment ids
// bd's import held; AddComment still mints its own.
func TestSeedCommentKeepsTheFixturesID(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := NewFake()
	is, err := f.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SeedComment("gt-a-comment", is.ID, "fixture", "seeded"); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	if err := f.AddComment(ctx, is.ID, "me", "minted"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	comments, err := f.Comments(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 || comments[0].ID != "gt-a-comment" || comments[0].Text != "seeded" {
		t.Fatalf("comments = %+v, want the seeded id first", comments)
	}
	if comments[1].ID == "gt-a-comment" {
		t.Errorf("AddComment reused the seeded id %q", comments[1].ID)
	}
	if err := f.SeedComment("gt-b-comment", "gt-missing", "fixture", "x"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("SeedComment on a missing issue = %v, want api.ErrNotFound", err)
	}
}

// mustFakeIssue reads one issue back from the fake.
func mustFakeIssue(t *testing.T, f *Fake, id string) api.Issue {
	t.Helper()
	got, err := f.GetIssues(t.Context(), []string{id})
	if err != nil {
		t.Fatal(err)
	}
	return got[id]
}

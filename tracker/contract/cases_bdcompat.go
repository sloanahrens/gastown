package contract

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// Cases that pin the behaviors callers written against bd rely on: the pin and
// assignee fences of the close verb, and what a read of one issue carries.

func init() {
	register(Case{Name: "a pinned issue is refused by close, passes update to closed, and force or release clears the pin", Run: bdPinnedFence})
	register(Case{Name: "close refuses an issue assigned to another actor but update to closed and force do not", Run: bdAssigneeCloseFence})
	register(Case{Name: "a close with no actor is refused on an assigned issue: the fence never fails open", Run: bdNoActorFence})
	register(Case{Name: "a pinned issue is not ready, is listed by default and does not block what depends on it", Run: bdPinnedElsewhere})
	register(Case{Name: "the close fence reads the blocking predicate: an open conditional blocker refuses, a closed or pinned one does not", Run: bdFenceReadsTheBlockingPredicate})
	register(Case{Name: "a batch close that includes a refused issue closes nothing", Run: bdBatchFenceAtomic})
	register(Case{Name: "a read of one issue carries its dependencies with relation, title and status, and its open blockers", Run: bdReadCarriesDependencies})
}

func asActor(actor string) context.Context { return api.WithActor(context.Background(), actor) }

func bdPinnedFence(t *testing.T, s store.Store) {
	pin := func(title string) api.Issue {
		is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: title})
		mustUpdate(t, s, is.ID, api.UpdatePatch{Status: str("pinned")}, api.Guard{})
		return is
	}
	p := pin("pinned")
	if err := s.CloseIssues(ctx(), []string{p.ID}, "r", false); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("closing a pinned issue = %v, want api.ErrConflict", err)
	}
	if got := mustGet(t, s, p.ID); got.Status != "pinned" {
		t.Errorf("a refused close left status %q, want pinned", got.Status)
	}
	// Release (back to open) clears the pin; the close then goes through.
	mustUpdate(t, s, p.ID, api.UpdatePatch{Status: str("open"), Assignee: str("")}, api.Guard{})
	if err := s.CloseIssues(ctx(), []string{p.ID}, "r", false); err != nil {
		t.Errorf("closing after the pin was cleared = %v, want nil", err)
	}
	// An update to status closed passes the pin fence; so does force.
	byUpdate := pin("pinned, closed by update")
	mustUpdate(t, s, byUpdate.ID, api.UpdatePatch{Status: str("closed")}, api.Guard{})
	if got := mustGet(t, s, byUpdate.ID); got.Status != "closed" {
		t.Errorf("update to closed of a pinned issue left status %q", got.Status)
	}
	forced := pin("pinned, force closed")
	if err := s.CloseIssues(ctx(), []string{forced.ID}, "forced", true); err != nil {
		t.Errorf("force close of a pinned issue = %v, want nil", err)
	}
}

func bdAssigneeCloseFence(t *testing.T, s store.Store) {
	alice := asActor("alice")
	theirs := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "bob's", Assignee: "bob"})
	if err := s.CloseIssues(alice, []string{theirs.ID}, "r", false); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("alice closing bob's issue = %v, want api.ErrConflict", err)
	}
	if got := mustGet(t, s, theirs.ID); got.Status != "open" {
		t.Errorf("a refused close left status %q, want open", got.Status)
	}
	mine := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "alice's", Assignee: "alice"})
	if err := s.CloseIssues(alice, []string{mine.ID}, "r", false); err != nil {
		t.Errorf("alice closing her own issue = %v, want nil", err)
	}
	// The fence is the close verb's alone: an update to status closed passes it, and force does too.
	viaUpdate := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "bob's, closed by update", Assignee: "bob"})
	if err := s.UpdateIssue(alice, viaUpdate.ID, api.UpdatePatch{Status: str("closed")}, api.Guard{}); err != nil {
		t.Errorf("update to closed of bob's issue = %v, want nil (the assignee fence is close's alone)", err)
	}
	forced := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "bob's, forced", Assignee: "bob"})
	if err := s.CloseIssues(alice, []string{forced.ID}, "r", true); err != nil {
		t.Errorf("force close of bob's issue = %v, want nil", err)
	}
	// An unassigned issue closes for anyone.
	free := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "unassigned"})
	if err := s.CloseIssues(alice, []string{free.ID}, "r", false); err != nil {
		t.Errorf("closing an unassigned issue = %v, want nil", err)
	}
}

func bdReadCarriesDependencies(t *testing.T, s store.Store) {
	work := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "work"})
	open := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "open blocker"})
	done := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "closed blocker"})
	tracked := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "tracked"})
	mustDep(t, s, work.ID, open.ID, "blocks")
	mustDep(t, s, work.ID, done.ID, "blocks")
	mustDep(t, s, work.ID, tracked.ID, "tracks")
	mustDep(t, s, work.ID, "external:hq:hq-abc", "tracks")
	if err := s.CloseIssues(ctx(), []string{done.ID}, "r", true); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, s, work.ID)
	byID := map[string]api.IssueDep{}
	for _, d := range got.Dependencies {
		byID[d.ID] = d
	}
	if len(got.Dependencies) != 3 {
		t.Fatalf("Dependencies = %+v, want the three local targets and no external one", got.Dependencies)
	}
	if d := byID[open.ID]; d.DependencyType != "blocks" || d.Title != "open blocker" || d.Status != "open" {
		t.Errorf("open blocker dependency = %+v", d)
	}
	if d := byID[done.ID]; d.DependencyType != "blocks" || d.Status != "closed" {
		t.Errorf("closed blocker dependency = %+v, want status closed", d)
	}
	if d := byID[tracked.ID]; d.DependencyType != "tracks" {
		t.Errorf("tracked dependency = %+v, want relation tracks", d)
	}
	if len(got.BlockedBy) != 1 || got.BlockedBy[0] != open.ID {
		t.Errorf("BlockedBy = %v, want only the open blocker %s (tracks does not block, closed does not count)", got.BlockedBy, open.ID)
	}
	if bare := mustGet(t, s, tracked.ID); len(bare.Dependencies) != 0 || len(bare.BlockedBy) != 0 {
		t.Errorf("an issue with no outgoing links carries %+v / %v, want none", bare.Dependencies, bare.BlockedBy)
	}
	// The other end of the links: what depends on an issue, with the relation.
	if tr := mustGet(t, s, tracked.ID); len(tr.Dependents) != 1 || tr.Dependents[0].ID != work.ID || tr.Dependents[0].DependencyType != "tracks" || tr.Dependents[0].Title != "work" {
		t.Errorf("tracked.Dependents = %+v, want work, relation tracks, with its title", tr.Dependents)
	}
	if ob := mustGet(t, s, open.ID); len(ob.Dependents) != 1 || ob.Dependents[0].ID != work.ID || ob.Dependents[0].DependencyType != "blocks" {
		t.Errorf("open blocker Dependents = %+v, want work, relation blocks", ob.Dependents)
	}
	if w := mustGet(t, s, work.ID); len(w.Dependents) != 0 {
		t.Errorf("work.Dependents = %+v, want none: nothing depends on it", w.Dependents)
	}
}

func bdNoActorFence(t *testing.T, s store.Store) {
	// The server always supplies the token's actor; a caller that reaches the
	// store with none must not be able to close someone's claim.
	theirs := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "claimed", Assignee: "bob"})
	if err := s.CloseIssues(ctx(), []string{theirs.ID}, "r", false); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("closing an assigned issue with no actor = %v, want api.ErrConflict", err)
	}
	free := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "unclaimed"})
	if err := s.CloseIssues(ctx(), []string{free.ID}, "r", false); err != nil {
		t.Errorf("closing an unassigned issue with no actor = %v, want nil", err)
	}
}

func bdPinnedElsewhere(t *testing.T, s store.Store) {
	pinned := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "pinned"})
	mustUpdate(t, s, pinned.ID, api.UpdatePatch{Status: str("pinned")}, api.Guard{})
	waiter := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "depends on the pinned one"})
	mustDep(t, s, waiter.ID, pinned.ID, "blocks")
	ready := ids(mustReady(t, s, api.ReadyFilter{}))
	if slices.Contains(ready, pinned.ID) {
		t.Errorf("ready holds the pinned issue %s: it is not open", pinned.ID)
	}
	// bd's predicate: a blocker that is closed or pinned no longer blocks.
	if !slices.Contains(ready, waiter.ID) {
		t.Errorf("ready = %v lacks %s: a pinned blocker does not block", ready, waiter.ID)
	}
	listed := false
	for _, is := range mustList(t, s, api.ListFilter{}).Issues {
		listed = listed || is.ID == pinned.ID
	}
	if !listed {
		t.Error("the default list leaves out a pinned issue; only closed issues are left out")
	}
	if got := mustGet(t, s, waiter.ID); len(got.BlockedBy) != 0 {
		t.Errorf("BlockedBy = %v, want none: the only blocker is pinned", got.BlockedBy)
	}
	// The close fence reads the same predicate: a pinned blocker does not stop a close.
	if err := s.CloseIssues(ctx(), []string{waiter.ID}, "r", false); err != nil {
		t.Errorf("closing an issue whose only blocker is pinned = %v, want nil", err)
	}
}

func bdBatchFenceAtomic(t *testing.T, s store.Store) {
	free := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "closable"})
	pinned := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "pinned"})
	mustUpdate(t, s, pinned.ID, api.UpdatePatch{Status: str("pinned")}, api.Guard{})
	for _, order := range [][]string{{free.ID, pinned.ID}, {pinned.ID, free.ID}} {
		if err := s.CloseIssues(ctx(), order, "r", false); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("CloseIssues(%v) = %v, want api.ErrConflict", order, err)
		}
		if got := mustGet(t, s, free.ID); got.Status != "open" {
			t.Errorf("after the refused batch %v the closable issue is %q, want open (the store closes all or none)", order, got.Status)
		}
	}
}

func bdFenceReadsTheBlockingPredicate(t *testing.T, s store.Store) {
	for _, typ := range []string{"blocks", "conditional-blocks"} {
		blocker := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: typ + " blocker"})
		held := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "held by " + typ})
		mustDep(t, s, held.ID, blocker.ID, typ)
		if err := s.CloseIssues(ctx(), []string{held.ID}, "r", false); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("closing an issue with an open %s blocker = %v, want api.ErrConflict", typ, err)
		}
		if got := mustGet(t, s, held.ID); len(got.BlockedBy) != 1 || got.BlockedBy[0] != blocker.ID {
			t.Errorf("%s: BlockedBy = %v, want the open blocker", typ, got.BlockedBy)
		}
		if err := s.CloseIssues(ctx(), []string{blocker.ID}, "r", false); err != nil {
			t.Fatal(err)
		}
		if err := s.CloseIssues(ctx(), []string{held.ID}, "r", false); err != nil {
			t.Errorf("closing after the %s blocker closed = %v, want nil", typ, err)
		}
		pinnedBlocker := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: typ + " pinned blocker"})
		mustUpdate(t, s, pinnedBlocker.ID, api.UpdatePatch{Status: str("pinned")}, api.Guard{})
		other := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "held by a pinned " + typ})
		mustDep(t, s, other.ID, pinnedBlocker.ID, typ)
		if err := s.CloseIssues(ctx(), []string{other.ID}, "r", false); err != nil {
			t.Errorf("closing an issue whose %s blocker is pinned = %v, want nil", typ, err)
		}
	}
}

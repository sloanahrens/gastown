package contract

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// Cases that pin the behaviors callers written against bd rely on: the pin and
// assignee fences of the close verb, and what a read of one issue carries.

func init() {
	register(Case{Name: "a pinned issue is refused by close, passes update to closed, and force or release clears the pin", Run: bdPinnedFence})
	register(Case{Name: "close refuses an issue assigned to another actor but update to closed and force do not", Run: bdAssigneeCloseFence})
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
}

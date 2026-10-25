package contract

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "close records the reason and time, and a closed issue leaves the default list", Run: closeBasics})
	register(Case{Name: "close refuses open children unless forced", Run: closeOpenChildren})
	register(Case{Name: "close is all-or-nothing and tolerates an already closed issue", Run: closeAllOrNothing})
	register(Case{Name: "closing a parent together with its children, or a blocked issue with its blocker, works in one call", Run: closeSetTogether})
	register(Case{Name: "update to closed obeys the same fences and reopen clears the close", Run: closeViaUpdateAndReopen})
	register(Case{Name: "delete removes issues, orphans their children, and is all-or-nothing", Run: deleteBasics})
}

func closeBasics(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	if err := s.CloseIssues(ctx(), []string{a.ID}, "done: shipped", false); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, s, a.ID)
	if got.Status != "closed" || got.CloseReason != "done: shipped" || got.ClosedAt == nil {
		t.Errorf("after close: %+v, want closed with a reason and a time", got)
	}
	sameSet(t, "default list", ids(mustList(t, s, api.ListFilter{})))
	sameSet(t, "closed list", ids(mustList(t, s, api.ListFilter{Statuses: []string{"closed"}})), a.ID)
	sameSet(t, "all list", ids(mustList(t, s, api.ListFilter{Statuses: []string{"all"}})), a.ID)
	sameSet(t, "closed_after the close", ids(mustList(t, s, api.ListFilter{Statuses: []string{"closed"}, ClosedAfter: got.ClosedAt.Add(-1)})), a.ID)
	sameSet(t, "closed_after the future", ids(mustList(t, s, api.ListFilter{Statuses: []string{"closed"}, ClosedAfter: got.ClosedAt.Add(60e9)})))
}

func closeOpenChildren(t *testing.T, s store.Store) {
	parent := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "epic"})
	kid := mustCreate(t, s, api.CreateParams{Parent: parent.ID, Title: "kid"})
	err := s.CloseIssues(ctx(), []string{parent.ID}, "r", false)
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("closing a parent with an open child = %v, want api.ErrConflict", err)
	}
	if got := mustGet(t, s, parent.ID); got.Status != "open" {
		t.Errorf("the refused close changed the parent: %+v", got)
	}
	if err := s.CloseIssues(ctx(), []string{kid.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseIssues(ctx(), []string{parent.ID}, "r", false); err != nil {
		t.Errorf("closing the parent after its child closed: %v", err)
	}
	p2 := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "epic2"})
	mustCreate(t, s, api.CreateParams{Parent: p2.ID, Title: "kid2"})
	if err := s.CloseIssues(ctx(), []string{p2.ID}, "forced", true); err != nil {
		t.Errorf("a forced close past an open child: %v", err)
	}
}

func closeAllOrNothing(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	b := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b"})
	if err := s.CloseIssues(ctx(), []string{a.ID, "gt-missing"}, "r", false); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("closing with a missing id = %v, want api.ErrNotFound", err)
	}
	if got := mustGet(t, s, a.ID); got.Status != "open" {
		t.Errorf("%s was closed although the batch failed", a.ID)
	}
	if err := s.CloseIssues(ctx(), []string{a.ID, b.ID}, "both", false); err != nil {
		t.Fatal(err)
	}
	first := mustGet(t, s, a.ID).ClosedAt
	if err := s.CloseIssues(ctx(), []string{a.ID}, "again", false); err != nil {
		t.Errorf("closing a closed issue: %v (it is skipped, not an error)", err)
	}
	if got := mustGet(t, s, a.ID); got.CloseReason != "both" || !got.ClosedAt.Equal(*first) {
		t.Errorf("a second close rewrote the first: %+v", got)
	}
	if err := s.CloseIssues(ctx(), nil, "r", false); err != nil {
		t.Errorf("closing nothing: %v", err)
	}
}

func closeViaUpdateAndReopen(t *testing.T, s store.Store) {
	parent := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "epic"})
	mustCreate(t, s, api.CreateParams{Parent: parent.ID, Title: "kid"})
	if err := s.UpdateIssue(ctx(), parent.ID, api.UpdatePatch{Status: str("closed")}, api.Guard{}); !errors.Is(err, api.ErrConflict) {
		t.Errorf("update to closed past an open child = %v, want api.ErrConflict", err)
	}
	solo := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "solo"})
	mustUpdate(t, s, solo.ID, api.UpdatePatch{Status: str("closed")}, api.Guard{})
	if got := mustGet(t, s, solo.ID); got.Status != "closed" || got.ClosedAt == nil {
		t.Fatalf("update to closed: %+v", got)
	}
	mustUpdate(t, s, solo.ID, api.UpdatePatch{Status: str("open")}, api.Guard{})
	got := mustGet(t, s, solo.ID)
	if got.Status != "open" || got.ClosedAt != nil || got.CloseReason != "" {
		t.Errorf("after reopen: %+v, want open with no close time or reason", got)
	}
}

func deleteBasics(t *testing.T, s store.Store) {
	parent := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "parent", Labels: []string{"x"}})
	kid := mustCreate(t, s, api.CreateParams{Parent: parent.ID, Title: "kid"})
	other := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "other"})
	if err := s.DeleteIssues(ctx(), []string{parent.ID, "gt-missing"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("deleting with a missing id = %v, want api.ErrNotFound", err)
	}
	if _, ok := mustGetMaybe(t, s, parent.ID); !ok {
		t.Fatal("a failed delete batch removed an issue")
	}
	if err := s.DeleteIssues(ctx(), []string{parent.ID}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustGetMaybe(t, s, parent.ID); ok {
		t.Error("the parent still exists after delete")
	}
	if k, ok := mustGetMaybe(t, s, kid.ID); !ok || k.Parent != "" {
		t.Errorf("child after its parent was deleted = %+v (exists %v), want it kept with no parent", k, ok)
	}
	if _, ok := mustGetMaybe(t, s, other.ID); !ok {
		t.Error("an unrelated issue was deleted")
	}
	sameSet(t, "list after delete", ids(mustList(t, s, api.ListFilter{})), kid.ID, other.ID)
}

func mustGetMaybe(t *testing.T, s store.Store, id string) (api.Issue, bool) {
	t.Helper()
	got, err := s.GetIssues(ctx(), []string{id})
	if err != nil {
		t.Fatal(err)
	}
	is, ok := got[id]
	return is, ok
}

func closeSetTogether(t *testing.T, s store.Store) {
	parent := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "parent"})
	kid := mustCreate(t, s, api.CreateParams{Parent: parent.ID, Title: "kid"})
	grand := mustCreate(t, s, api.CreateParams{Parent: kid.ID, Title: "grand"})
	// Listed parent-first, which is also id order: the set must not depend on it.
	if err := s.CloseIssues(ctx(), []string{parent.ID, kid.ID, grand.ID}, "all done", false); err != nil {
		t.Fatalf("closing a whole subtree in one call: %v", err)
	}
	for _, id := range []string{parent.ID, kid.ID, grand.ID} {
		if got := mustGet(t, s, id); got.Status != "closed" {
			t.Errorf("%s is %s after the batch, want closed", id, got.Status)
		}
	}
	blocker := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "blocker"})
	work := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "work"})
	mustDep(t, s, work.ID, blocker.ID, "blocks")
	if err := s.CloseIssues(ctx(), []string{work.ID, blocker.ID}, "both", false); err != nil {
		t.Errorf("closing an issue together with its blocker: %v", err)
	}
	// A child left out of the set still fences its parent.
	p2 := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "p2"})
	mustCreate(t, s, api.CreateParams{Parent: p2.ID, Title: "left out"})
	other := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "other"})
	if err := s.CloseIssues(ctx(), []string{p2.ID, other.ID}, "r", false); !errors.Is(err, api.ErrConflict) {
		t.Errorf("closing a parent without its open child = %v, want api.ErrConflict", err)
	}
	if got := mustGet(t, s, other.ID); got.Status != "open" {
		t.Error("the refused batch closed an unrelated member")
	}
}

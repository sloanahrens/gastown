package contract

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "update changes fields, notes and labels and bumps updated_at", Run: updateFields})
	register(Case{Name: "update refuses an empty title, an unknown status and a missing issue", Run: updateRefusals})
	register(Case{Name: "update guards: a guard that does not hold writes nothing", Run: updateGuards})
	register(Case{Name: "two racing claims with a guard have exactly one winner", Run: updateClaimRace})
	register(Case{Name: "another actor's live in_progress claim is not overwritten without a guard or force", Run: updateClaimFence})
	register(Case{Name: "update persistent promotes a wisp", Run: updatePersistent})
}

func str(s string) *string { return &s }

func mustGet(t *testing.T, s store.Store, id string) api.Issue {
	t.Helper()
	got, err := s.GetIssues(ctx(), []string{id})
	if err != nil {
		t.Fatal(err)
	}
	is, ok := got[id]
	if !ok {
		t.Fatalf("issue %s is missing", id)
	}
	return is
}

func mustUpdate(t *testing.T, s store.Store, id string, p api.UpdatePatch, g api.Guard) {
	t.Helper()
	if err := s.UpdateIssue(ctx(), id, p, g); err != nil {
		t.Fatalf("UpdateIssue(%s, %+v, %+v): %v", id, p, g, err)
	}
}

func updateFields(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "old", Priority: 2, Labels: []string{"a", "b"}})
	prio := 0
	mustUpdate(t, s, is.ID, api.UpdatePatch{
		Title: str("new"), Description: str("d"), Acceptance: str("- [ ] x"), Priority: &prio, Assignee: str("me"),
		Status: str("in_progress"), AppendNotes: "one", AddLabels: []string{"c"}, RemoveLabels: []string{"a"},
	}, api.Guard{})
	mustUpdate(t, s, is.ID, api.UpdatePatch{AppendNotes: "two"}, api.Guard{})
	got := mustGet(t, s, is.ID)
	if got.Title != "new" || got.Description != "d" || got.AcceptanceCriteria != "- [ ] x" || got.Priority != 0 || got.Assignee != "me" || got.Status != "in_progress" {
		t.Errorf("after update: %+v", got)
	}
	if got.Notes != "one\ntwo" {
		t.Errorf("Notes = %q, want the two notes on separate lines", got.Notes)
	}
	if !slices.Equal(got.Labels, []string{"b", "c"}) {
		t.Errorf("Labels = %v, want [b c]", got.Labels)
	}
	if !got.UpdatedAt.After(is.UpdatedAt) {
		t.Errorf("UpdatedAt %v not after %v", got.UpdatedAt, is.UpdatedAt)
	}
	mustUpdate(t, s, is.ID, api.UpdatePatch{SetLabels: []string{"z"}}, api.Guard{})
	if got := mustGet(t, s, is.ID); !slices.Equal(got.Labels, []string{"z"}) {
		t.Errorf("after SetLabels Labels = %v, want [z]", got.Labels)
	}
	before := mustGet(t, s, is.ID).UpdatedAt
	mustUpdate(t, s, is.ID, api.UpdatePatch{Title: str("new")}, api.Guard{}) // no change
	if after := mustGet(t, s, is.ID).UpdatedAt; !after.Equal(before) {
		t.Errorf("a patch that changes nothing moved UpdatedAt from %v to %v", before, after)
	}
}

func updateRefusals(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "t"})
	if err := s.UpdateIssue(ctx(), is.ID, api.UpdatePatch{Title: str("  ")}, api.Guard{}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("empty title error = %v, want api.ErrInvalid", err)
	}
	if err := s.UpdateIssue(ctx(), is.ID, api.UpdatePatch{Status: str("wip")}, api.Guard{}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("unknown status error = %v, want api.ErrInvalid", err)
	}
	if err := s.UpdateIssue(ctx(), "gt-nope", api.UpdatePatch{Title: str("x")}, api.Guard{}); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("missing issue error = %v, want api.ErrNotFound", err)
	}
	if got := mustGet(t, s, is.ID); got.Title != "t" || got.Status != "open" {
		t.Errorf("a refused update changed the issue: %+v", got)
	}
}

func updateGuards(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "t", Assignee: "alice"})
	err := s.UpdateIssue(ctx(), is.ID, api.UpdatePatch{Title: str("hijacked"), Assignee: str("bob")}, api.Guard{IfAssignee: str("carol")})
	if !errors.Is(err, api.ErrGuardFailed) {
		t.Fatalf("wrong if_assignee error = %v, want api.ErrGuardFailed", err)
	}
	err = s.UpdateIssue(ctx(), is.ID, api.UpdatePatch{Title: str("x")}, api.Guard{IfStatus: str("in_progress")})
	if !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("wrong if_status error = %v, want api.ErrGuardFailed", err)
	}
	if got := mustGet(t, s, is.ID); got.Title != "t" || got.Assignee != "alice" {
		t.Errorf("a failed guard wrote something: %+v", got)
	}
	mustUpdate(t, s, is.ID, api.UpdatePatch{Assignee: str("")}, api.Guard{IfAssignee: str("alice"), IfStatus: str("open")})
	if got := mustGet(t, s, is.ID); got.Assignee != "" {
		t.Errorf("Assignee = %q after a release that held its guard, want empty", got.Assignee)
	}
}

func updateClaimRace(t *testing.T, s store.Store) {
	// Many rounds, each with every claimer released at the same instant, so a
	// guard checked outside the row lock shows up as a round with two winners.
	const rounds, claimers = 25, 8
	for round := 0; round < rounds; round++ {
		is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "contested"})
		errs := make([]error, claimers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < claimers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				who := fmt.Sprintf("claimer-%d", i)
				<-start
				errs[i] = s.UpdateIssue(ctx(), is.ID, api.UpdatePatch{Assignee: str(who), Status: str("in_progress"), Actor: who}, api.Guard{IfAssignee: str("")})
			}(i)
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, api.ErrGuardFailed):
			default:
				t.Errorf("round %d: unexpected error %v", round, err)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d claims won, want exactly 1 (the guard is checked under the row lock)", round, wins)
		}
		if got := mustGet(t, s, is.ID); got.Assignee == "" || got.Status != "in_progress" {
			t.Fatalf("round %d: %+v, want a single claimant holding it in_progress", round, got)
		}
	}
}

func updateClaimFence(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "held", Assignee: "alice"})
	mustUpdate(t, s, is.ID, api.UpdatePatch{Status: str("in_progress"), Actor: "alice"}, api.Guard{})
	if err := s.UpdateIssue(ctx(), is.ID, api.UpdatePatch{Assignee: str("bob"), Actor: "bob"}, api.Guard{}); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("overwriting another actor's live claim = %v, want api.ErrGuardFailed", err)
	}
	if err := s.UpdateIssue(ctx(), is.ID, api.UpdatePatch{Status: str("open"), Assignee: str(""), Actor: "bob"}, api.Guard{}); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("another actor releasing a live claim = %v, want api.ErrGuardFailed", err)
	}
	mustUpdate(t, s, is.ID, api.UpdatePatch{AppendNotes: "alice works", Actor: "alice"}, api.Guard{})
	mustUpdate(t, s, is.ID, api.UpdatePatch{Assignee: str("carol"), Actor: "bob", Force: true}, api.Guard{})
	if got := mustGet(t, s, is.ID); got.Assignee != "carol" {
		t.Errorf("Assignee = %q after a forced reassignment, want carol", got.Assignee)
	}
	mustUpdate(t, s, is.ID, api.UpdatePatch{Assignee: str("dave"), Actor: "erin"}, api.Guard{IfAssignee: str("carol")})
	if got := mustGet(t, s, is.ID); got.Assignee != "dave" {
		t.Errorf("Assignee = %q after a guarded reassignment, want dave (the guard replaces the fence)", got.Assignee)
	}
}

func updatePersistent(t *testing.T, s store.Store) {
	w := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "w", Ephemeral: true})
	mustUpdate(t, s, w.ID, api.UpdatePatch{Persistent: true}, api.Guard{})
	if got := mustGet(t, s, w.ID); got.Ephemeral {
		t.Error("the issue is still ephemeral after Persistent")
	}
}

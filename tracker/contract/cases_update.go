package contract

import (
	"context"
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
	register(Case{Name: "a reassignment needs the holder on every status, and only an admin token takes a held issue", Run: updateReassignHolder})
	register(Case{Name: "update persistent promotes a wisp", Run: updatePersistent})
	register(Case{Name: "a status write alone obeys the guards, the close fences and the reopen clear", Fake: true, Run: updateStatusOnly})
	register(Case{Name: "update writes the title, description, acceptance and appended notes the shim's delete and defer send", Fake: true, Run: updateTextFields})
}

// updateStatusOnly is the status write the bd shim's update, close and reopen
// handlers make (gt-p85pw.96), and the fences that answer them: the fake models
// this write and nothing more of a patch, so the case is flagged Fake.
func updateStatusOnly(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "t"})
	mustUpdate(t, s, is.ID, api.UpdatePatch{Status: str("in_progress")}, api.Guard{})
	if got := mustGet(t, s, is.ID); got.Status != "in_progress" {
		t.Errorf("status = %q after the update, want in_progress", got.Status)
	}
	if err := s.UpdateIssue(ctx(), is.ID, api.UpdatePatch{Status: str("wip")}, api.Guard{}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("an unknown status = %v, want api.ErrInvalid", err)
	}
	if err := s.UpdateIssue(ctx(), is.ID, api.UpdatePatch{Status: str("open")}, api.Guard{IfStatus: str("open")}); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("a guard that did not hold = %v, want api.ErrGuardFailed", err)
	}
	if got := mustGet(t, s, is.ID); got.Status != "in_progress" {
		t.Errorf("a refused status write changed the issue: %+v", got)
	}

	blocker := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "blocker"})
	work := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "blocked"})
	mustDep(t, s, work.ID, blocker.ID, "blocks")
	if err := s.UpdateIssue(ctx(), work.ID, api.UpdatePatch{Status: str("closed")}, api.Guard{}); !errors.Is(err, api.ErrConflict) {
		t.Errorf("closing a blocked issue = %v, want api.ErrConflict", err)
	}
	mustUpdate(t, s, work.ID, api.UpdatePatch{Status: str("closed"), Force: true}, api.Guard{})
	if got := mustGet(t, s, work.ID); got.Status != "closed" || got.ClosedAt == nil {
		t.Errorf("after a forced close: %+v", got)
	}
	mustUpdate(t, s, work.ID, api.UpdatePatch{Status: str("open")}, api.Guard{})
	if got := mustGet(t, s, work.ID); got.Status != "open" || got.ClosedAt != nil || got.CloseReason != "" {
		t.Errorf("after the reopen: %+v, want open with no close time or reason", got)
	}

	parent := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "epic"})
	mustCreate(t, s, api.CreateParams{Rig: "gt", Parent: parent.ID, Title: "kid"})
	if err := s.CloseIssues(ctx(), []string{parent.ID}, "r", false); !errors.Is(err, api.ErrConflict) {
		t.Errorf("closing a parent with an open child = %v, want api.ErrConflict", err)
	}
	if err := s.CloseIssues(ctx(), []string{parent.ID}, "r", true); err != nil {
		t.Errorf("forcing the parent closed = %v, want nil", err)
	}
}

// updateTextFields is the text write the bd shim's delete and defer handlers
// make (gt-p85pw.97): delete rewrites references in title, description and
// acceptance, and defer appends its reason to the notes. The fake models these
// fields for those handlers' unit tests, so the case is flagged Fake.
func updateTextFields(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "old", Description: "first draft", Acceptance: "- [ ] one"})
	mustUpdate(t, s, is.ID, api.UpdatePatch{
		Title: str("new"), Description: str("second draft"), Acceptance: str("- [x] one"), AppendNotes: "note one",
	}, api.Guard{})
	mustUpdate(t, s, is.ID, api.UpdatePatch{AppendNotes: "note two"}, api.Guard{})
	got := mustGet(t, s, is.ID)
	if got.Title != "new" || got.Description != "second draft" || got.AcceptanceCriteria != "- [x] one" {
		t.Errorf("after the text writes: %+v", got)
	}
	if got.Notes != "note one\nnote two" {
		t.Errorf("Notes = %q, want the two appended notes on separate lines", got.Notes)
	}
	before := got.UpdatedAt
	mustUpdate(t, s, is.ID, api.UpdatePatch{Title: str("new")}, api.Guard{})
	if after := mustGet(t, s, is.ID).UpdatedAt; !after.Equal(before) {
		t.Errorf("a patch that changes nothing moved UpdatedAt from %v to %v", before, after)
	}
	if err := s.UpdateIssue(ctx(), is.ID, api.UpdatePatch{Title: str("   ")}, api.Guard{}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("a whitespace title = %v, want api.ErrInvalid", err)
	}
	if got := mustGet(t, s, is.ID); got.Title != "new" {
		t.Errorf("a refused title write left title %q, want new", got.Title)
	}
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
	mustUpdate(t, s, is.ID, api.UpdatePatch{Assignee: str(""), Actor: "alice"}, api.Guard{IfAssignee: str("alice"), IfStatus: str("open")})
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
	if err := s.UpdateIssue(as("bob"), is.ID, api.UpdatePatch{Assignee: str("carol"), Force: true}, api.Guard{}); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("a seat's forced reassignment of a live claim = %v, want api.ErrGuardFailed", err)
	}
	admin := api.WithIdentity(ctx(), api.Identity{Actor: "town/daemon", Scope: api.ScopeAdmin})
	if err := s.UpdateIssue(admin, is.ID, api.UpdatePatch{Assignee: str("carol"), Force: true}, api.Guard{}); err != nil {
		t.Fatalf("an admin's forced reassignment = %v, want nil", err)
	}
	if got := mustGet(t, s, is.ID); got.Assignee != "carol" {
		t.Errorf("Assignee = %q after a forced reassignment, want carol", got.Assignee)
	}
	if err := s.UpdateIssue(as("erin"), is.ID, api.UpdatePatch{Assignee: str("dave")}, api.Guard{IfAssignee: str("carol")}); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("a guard naming the holder let another actor reassign = %v, want api.ErrGuardFailed", err)
	}
	mustUpdate(t, s, is.ID, api.UpdatePatch{Assignee: str("dave"), Actor: "carol"}, api.Guard{IfAssignee: str("carol")})
	if got := mustGet(t, s, is.ID); got.Assignee != "dave" {
		t.Errorf("Assignee = %q after the holder's guarded reassignment, want dave", got.Assignee)
	}
}

// updateReassignHolder holds a reassignment to the issue's holder on every
// status. A guard and a seat's force do not make the caller the holder; only
// an admin-scope token takes a held issue, with or without force.
func updateReassignHolder(t *testing.T, s store.Store) {
	admin := api.WithIdentity(ctx(), api.Identity{Actor: "town/daemon", Scope: api.ScopeAdmin})
	for _, tc := range []struct {
		name    string
		holder  string // the assignee at setup; "" for none
		status  string // "open", "in_progress" or "closed" at setup
		ctx     context.Context
		patch   api.UpdatePatch
		guard   api.Guard
		refused bool
		want    string // the assignee after the patch
	}{
		{name: "an open issue held by another seat is refused", holder: "alice", status: "open", ctx: as("bob"), patch: api.UpdatePatch{Assignee: str("bob")}, refused: true, want: "alice"},
		{name: "an in_progress issue held by another seat is refused", holder: "alice", status: "in_progress", ctx: as("bob"), patch: api.UpdatePatch{Assignee: str("bob")}, refused: true, want: "alice"},
		{name: "a closed issue held by another seat is refused", holder: "alice", status: "closed", ctx: as("bob"), patch: api.UpdatePatch{Assignee: str("bob")}, refused: true, want: "alice"},
		{name: "a guard naming the holder does not authorize another seat", holder: "alice", status: "open", ctx: as("bob"), patch: api.UpdatePatch{Assignee: str("bob")},
			guard: api.Guard{IfAssignee: str("alice")}, refused: true, want: "alice"},
		{name: "a seat's force does not authorize another seat", holder: "alice", status: "in_progress", ctx: as("bob"), patch: api.UpdatePatch{Assignee: str("bob"), Force: true}, refused: true, want: "alice"},
		{name: "an admin's force takes an abandoned claim", holder: "alice", status: "in_progress", ctx: admin, patch: api.UpdatePatch{Assignee: str("bob"), Force: true}, want: "bob"},
		{name: "an admin takes a held issue without force, as the daemon's dead-holder reopen does", holder: "alice", status: "open", ctx: admin,
			patch: api.UpdatePatch{Assignee: str("bob")}, guard: api.Guard{IfAssignee: str("alice")}, want: "bob"},
		{name: "an unheld issue can be assigned", holder: "", status: "open", ctx: as("bob"), patch: api.UpdatePatch{Assignee: str("bob")}, want: "bob"},
		{name: "the holder can reassign its issue", holder: "alice", status: "open", ctx: as("alice"), patch: api.UpdatePatch{Assignee: str("bob")}, want: "bob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "held", Assignee: tc.holder})
			switch tc.status {
			case "in_progress":
				if err := s.Claim(as(tc.holder), is.ID); err != nil {
					t.Fatalf("claim = %v, want nil", err)
				}
			case "closed":
				if err := s.CloseIssues(as(tc.holder), []string{is.ID}, "done", false); err != nil {
					t.Fatalf("close = %v, want nil", err)
				}
			}
			err := s.UpdateIssue(tc.ctx, is.ID, tc.patch, tc.guard)
			if tc.refused && !errors.Is(err, api.ErrGuardFailed) {
				t.Errorf("UpdateIssue = %v, want api.ErrGuardFailed", err)
			}
			if !tc.refused && err != nil {
				t.Errorf("UpdateIssue = %v, want nil", err)
			}
			if got := mustGet(t, s, is.ID); got.Assignee != tc.want {
				t.Errorf("assignee after the patch = %q, want %q", got.Assignee, tc.want)
			}
		})
	}
}

func updatePersistent(t *testing.T, s store.Store) {
	w := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "w", Ephemeral: true})
	mustUpdate(t, s, w.ID, api.UpdatePatch{Persistent: true}, api.Guard{})
	if got := mustGet(t, s, w.ID); got.Ephemeral {
		t.Error("the issue is still ephemeral after Persistent")
	}
}

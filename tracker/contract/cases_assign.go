package contract

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "claim takes an unheld issue for the caller and refuses another actor's hold", Run: claimGuard})
	register(Case{Name: "release and transfer refuse a caller that does not hold the issue and write nothing", Run: assignRefusals})
	register(Case{Name: "two racing claims have exactly one winner and the loser writes nothing", Run: claimRace})
	register(Case{Name: "the assignment verbs refuse without an actor and a lost claim does not name the holder", Fake: true, Run: assignIdentityAndHold})
	register(Case{Name: "claim, release and transfer refuse a closed issue and leave it closed", Run: assignClosed})
}

// as is the context of a request made by actor.
func as(actor string) context.Context { return api.WithActor(ctx(), actor) }

// assigneeWrites counts the events that record an assignee change.
func assigneeWrites(es []api.Event) int {
	n := 0
	for _, e := range es {
		if e.Comment != nil && *e.Comment == "assignee" {
			n++
		}
	}
	return n
}

func claimGuard(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "free"})
	if err := s.Claim(ctx(), is.ID); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("claim with no actor = %v, want api.ErrInvalid: there is nothing to guard on", err)
	}
	if got := mustGet(t, s, is.ID); got.Assignee != "" || got.Status != "open" {
		t.Errorf("an unguarded claim wrote: %+v", got)
	}
	if err := s.Claim(as("alice"), is.ID); err != nil {
		t.Fatalf("claim of an unheld issue = %v, want nil", err)
	}
	held := mustGet(t, s, is.ID)
	if held.Assignee != "alice" || held.Status != "in_progress" {
		t.Fatalf("after claim: assignee %q status %q, want alice and in_progress", held.Assignee, held.Status)
	}
	// The holder claiming again is not a loser, and it is not a write either.
	if err := s.Claim(as("alice"), is.ID); err != nil {
		t.Errorf("the holder re-claiming = %v, want nil", err)
	}
	if got := mustGet(t, s, is.ID); !got.UpdatedAt.Equal(held.UpdatedAt) {
		t.Errorf("the holder's re-claim moved UpdatedAt from %v to %v", held.UpdatedAt, got.UpdatedAt)
	}
	// A second actor loses the hold without writing.
	events := len(eventsFor(t, s, is.ID))
	if err := s.Claim(as("bob"), is.ID); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("another actor's claim = %v, want api.ErrGuardFailed", err)
	}
	if got := mustGet(t, s, is.ID); got.Assignee != "alice" || got.Status != "in_progress" || !got.UpdatedAt.Equal(held.UpdatedAt) {
		t.Errorf("a refused claim changed the issue: %+v", got)
	}
	if n := len(eventsFor(t, s, is.ID)); n != events {
		t.Errorf("a refused claim appended %d events", n-events)
	}

	// An issue assigned to another actor is held even before it is started, so
	// a claim cannot take it from that actor.
	assigned := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "reserved", Assignee: "alice"})
	if err := s.Claim(as("bob"), assigned.ID); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("claim of an issue assigned to another actor = %v, want api.ErrGuardFailed", err)
	}
	if got := mustGet(t, s, assigned.ID); got.Assignee != "alice" || got.Status != "open" {
		t.Errorf("a refused claim of an assigned issue changed it: %+v", got)
	}
}

func assignRefusals(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "held", Assignee: "alice"})
	// Held, not started: the update path's in_progress fence does not apply, so
	// only the holder guard can refuse another actor here.
	mustUpdate(t, s, is.ID, api.UpdatePatch{Status: str("hooked"), Actor: "alice"}, api.Guard{})
	held, events := mustGet(t, s, is.ID), len(eventsFor(t, s, is.ID))

	if err := s.Release(as("bob"), is.ID); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("release by an actor that does not hold it = %v, want api.ErrGuardFailed", err)
	}
	if err := s.Transfer(as("bob"), is.ID, "open", "bob"); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("transfer by an actor that does not hold it = %v, want api.ErrGuardFailed", err)
	}
	if err := s.Transfer(as("alice"), is.ID, "open", ""); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("transfer to no one = %v, want api.ErrInvalid (release leaves an issue unheld)", err)
	}
	if err := s.Release(ctx(), is.ID); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("release with no actor = %v, want api.ErrInvalid", err)
	}
	if err := s.Release(as("alice"), "gt-nope"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("release of a missing issue = %v, want api.ErrNotFound", err)
	}
	if got := mustGet(t, s, is.ID); got.Assignee != held.Assignee || got.Status != held.Status || !got.UpdatedAt.Equal(held.UpdatedAt) {
		t.Errorf("a refused release or transfer changed the issue: %+v", got)
	}
	if n := len(eventsFor(t, s, is.ID)); n != events {
		t.Errorf("a refused release or transfer appended %d events", n-events)
	}

	if err := s.Transfer(as("alice"), is.ID, "open", "bob"); err != nil {
		t.Fatalf("the holder's transfer = %v, want nil", err)
	}
	if got := mustGet(t, s, is.ID); got.Assignee != "bob" || got.Status != "open" {
		t.Errorf("after transfer: assignee %q status %q, want bob and open", got.Assignee, got.Status)
	}
	if err := s.Release(as("alice"), is.ID); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("the previous holder releasing after a transfer = %v, want api.ErrGuardFailed", err)
	}
	if err := s.Transfer(as("bob"), is.ID, "hooked", "carol"); err != nil {
		t.Fatalf("the new holder's transfer = %v, want nil", err)
	}
	if got := mustGet(t, s, is.ID); got.Assignee != "carol" || got.Status != "hooked" {
		t.Errorf("after transfer with a status: assignee %q status %q, want carol and hooked", got.Assignee, got.Status)
	}
	// A transfer that names no status keeps the one the issue has.
	if err := s.Transfer(as("carol"), is.ID, "", "dave"); err != nil {
		t.Fatalf("transfer without a status = %v, want nil", err)
	}
	if got := mustGet(t, s, is.ID); got.Assignee != "dave" || got.Status != "hooked" {
		t.Errorf("after transfer without a status: assignee %q status %q, want dave and hooked", got.Assignee, got.Status)
	}
	if err := s.Release(as("dave"), is.ID); err != nil {
		t.Fatalf("the holder's release = %v, want nil", err)
	}
	if got := mustGet(t, s, is.ID); got.Assignee != "" || got.Status != "open" {
		t.Errorf("after release: assignee %q status %q, want empty and open", got.Assignee, got.Status)
	}
}

func claimRace(t *testing.T, s store.Store) {
	// Both claims wait on one barrier, so a guard checked outside the row lock
	// shows up as a round with two winners.
	const rounds = 25
	seats := []string{"seat-a", "seat-b"}
	for round := 0; round < rounds; round++ {
		is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "contested"})
		errs := make([]error, len(seats))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, who := range seats {
			wg.Add(1)
			go func(i int, who string) {
				defer wg.Done()
				<-start
				errs[i] = s.Claim(as(who), is.ID)
			}(i, who)
		}
		close(start)
		wg.Wait()

		winner, losers := "", 0
		for i, err := range errs {
			switch {
			case err == nil:
				if winner != "" {
					t.Fatalf("round %d: %s and %s both won; the guard is not checked under the row lock", round, winner, seats[i])
				}
				winner = seats[i]
			case errors.Is(err, api.ErrGuardFailed):
				losers++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if winner == "" || losers != 1 {
			t.Fatalf("round %d: claim results %v, want exactly one winner", round, errs)
		}
		got := mustGet(t, s, is.ID)
		if got.Assignee != winner || got.Status != "in_progress" {
			t.Fatalf("round %d: %+v, want %s holding it in_progress", round, got, winner)
		}
		if n := assigneeWrites(eventsFor(t, s, is.ID)); n != 1 {
			t.Fatalf("round %d: %d assignee writes, want 1 (the loser wrote nothing)", round, n)
		}
	}
}

// assignIdentityAndHold is the fake-able part of the guard: every verb needs
// the actor on the context, and a verb by an actor that does not hold the
// issue is refused without a write.
func assignIdentityAndHold(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "held", Assignee: "alice"})
	for name, err := range map[string]error{
		"claim":    s.Claim(ctx(), is.ID),
		"release":  s.Release(ctx(), is.ID),
		"transfer": s.Transfer(ctx(), is.ID, "open", "bob"),
	} {
		if !errors.Is(err, api.ErrInvalid) {
			t.Errorf("%s with no actor = %v, want api.ErrInvalid", name, err)
		}
	}
	if err := s.Transfer(as("alice"), is.ID, "open", ""); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("transfer to no one = %v, want api.ErrInvalid", err)
	}
	for name, err := range map[string]error{
		"claim":    s.Claim(as("bob"), is.ID),
		"release":  s.Release(as("bob"), is.ID),
		"transfer": s.Transfer(as("bob"), is.ID, "open", "bob"),
	} {
		if !errors.Is(err, api.ErrGuardFailed) {
			t.Errorf("%s by an actor that does not hold it = %v, want api.ErrGuardFailed", name, err)
		}
	}
	for name, err := range map[string]error{
		"claim":    s.Claim(as("alice"), "gt-nope"),
		"release":  s.Release(as("alice"), "gt-nope"),
		"transfer": s.Transfer(as("alice"), "gt-nope", "open", "bob"),
	} {
		if !errors.Is(err, api.ErrNotFound) {
			t.Errorf("%s of a missing issue = %v, want api.ErrNotFound", name, err)
		}
	}
	// A losing claim is answered with the issue, never with who holds it: the
	// caller learns the race was lost without reading another seat's name.
	var guard *api.Error
	if err := s.Claim(as("bob"), is.ID); errors.As(err, &guard) {
		if !strings.Contains(guard.Message, is.ID) || strings.Contains(guard.Message, "alice") {
			t.Errorf("a lost claim says %q, want the issue id and not the holder", guard.Message)
		}
	} else {
		t.Errorf("a lost claim = %v, want an *api.Error", err)
	}
	if got := mustGet(t, s, is.ID); got.Assignee != "alice" || got.Status != "open" {
		t.Errorf("a refused verb changed the issue: %+v", got)
	}

	// The holder starts the issue it was given, and a repeat claim or a
	// transfer that changes nothing leaves the row (and its updated_at) alone.
	if err := s.Claim(as("alice"), is.ID); err != nil {
		t.Fatalf("the holder's claim of its open issue = %v, want nil", err)
	}
	started := mustGet(t, s, is.ID)
	if started.Assignee != "alice" || started.Status != "in_progress" {
		t.Fatalf("after the holder's claim: %+v, want alice in_progress", started)
	}
	if err := s.Claim(as("alice"), is.ID); err != nil {
		t.Errorf("the holder's repeat claim = %v, want nil", err)
	}
	if got := mustGet(t, s, is.ID); !got.UpdatedAt.Equal(started.UpdatedAt) {
		t.Errorf("a repeat claim moved UpdatedAt from %v to %v", started.UpdatedAt, got.UpdatedAt)
	}
	if err := s.Transfer(as("alice"), is.ID, "", "alice"); err != nil {
		t.Errorf("a transfer to the same holder at the same status = %v, want nil", err)
	}
	if got := mustGet(t, s, is.ID); !got.UpdatedAt.Equal(started.UpdatedAt) {
		t.Errorf("a transfer that changed nothing moved UpdatedAt from %v to %v", started.UpdatedAt, got.UpdatedAt)
	}
}

// assignClosed keeps the closed issue out of the verbs: a release or transfer
// would put it back in the live set, and a claim would reopen it.
func assignClosed(t *testing.T, s store.Store) {
	held := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "closed and held", Assignee: "alice"})
	if err := s.CloseIssues(as("alice"), []string{held.ID}, "done", false); err != nil {
		t.Fatal(err)
	}
	closed, events := mustGet(t, s, held.ID), len(eventsFor(t, s, held.ID))
	for name, err := range map[string]error{
		"claim":    s.Claim(as("alice"), held.ID),
		"release":  s.Release(as("alice"), held.ID),
		"transfer": s.Transfer(as("alice"), held.ID, "open", "bob"),
	} {
		if !errors.Is(err, api.ErrGuardFailed) {
			t.Errorf("%s of a closed issue = %v, want api.ErrGuardFailed", name, err)
		}
	}
	if got := mustGet(t, s, held.ID); got.Status != "closed" || got.Assignee != closed.Assignee || !got.UpdatedAt.Equal(closed.UpdatedAt) {
		t.Errorf("a refused verb reopened or moved a closed issue: %+v", got)
	}
	if n := len(eventsFor(t, s, held.ID)); n != events {
		t.Errorf("a refused verb on a closed issue appended %d events", n-events)
	}
	// The closed refusal is not the holder guard: an unheld closed issue is
	// refused too, and stays closed.
	free := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "closed and unheld"})
	if err := s.CloseIssues(ctx(), []string{free.ID}, "done", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Claim(as("bob"), free.ID); !errors.Is(err, api.ErrGuardFailed) {
		t.Errorf("claim of a closed unheld issue = %v, want api.ErrGuardFailed", err)
	}
	if got := mustGet(t, s, free.ID); got.Status != "closed" || got.Assignee != "" {
		t.Errorf("a refused claim reopened a closed issue: %+v", got)
	}
}

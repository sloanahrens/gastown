//go:build integration

package trackerclient_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/beads/trackerclient"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/server"
	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// newTrackerServer runs the real tracker (Postgres store, HTTP server, Go
// client) in-process on a fresh migrated database. It returns a function that
// mints a beads.Client over that one server, acting as actor with scope, so
// several roles can share one database. Integration tier: one clone of a
// template database, about 70 ms, plus a loopback server.
func newTrackerServer(t *testing.T) func(actor string, scope api.Scope) *trackerclient.Client {
	t.Helper()
	pool := pgtest.New(t, "migrated")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown'), ('hq', 'town')"); err != nil {
		t.Fatal(err)
	}
	pg := store.NewPostgres(pool)
	srv := httptest.NewServer(server.New(server.Config{Store: pg, Auth: pg, Health: pg, SchemaLevel: store.SchemaLevel()}).Handler())
	t.Cleanup(srv.Close)
	return func(actor string, scope api.Scope) *trackerclient.Client {
		t.Helper()
		token, err := pg.IssueTokenScoped(ctx, actor, scope, nil)
		if err != nil {
			t.Fatal(err)
		}
		tc, err := client.New(client.Config{BaseURL: srv.URL, Token: token, SchemaLevel: store.SchemaLevel()})
		if err != nil {
			t.Fatal(err)
		}
		return trackerclient.New(tc, "gt")
	}
}

// newTracker returns a beads.Client over a fresh tracker acting as actor.
func newTracker(t *testing.T, actor string, scope api.Scope) *trackerclient.Client {
	t.Helper()
	return newTrackerServer(t)(actor, scope)
}

// newTrackerRoles returns the roles one contract case acts through, all on one
// tracker: the seat the suite runs as, the admin-scope daemon, and a seat for
// each actor named as a holder. The tracker refuses a seat a reassignment of a
// held issue and a delete (gt-26dvk), so those rows act through the role the
// tracker grants them.
func newTrackerRoles(t *testing.T) beadsfake.Roles {
	t.Helper()
	mint := newTrackerServer(t)
	return beadsfake.Roles{
		Seat:   mint("town/crew/dipper", api.ScopeSeat),
		Admin:  mint("town/daemon", api.ScopeAdmin),
		Holder: func(actor string) beads.Client { return mint(actor, api.ScopeSeat) },
	}
}

// The adapter must behave like bd on every case the fake and bd are held to,
// except two suites it cannot run. RunNotesFilterContract reads notes and
// update time, which the tracker's list query cannot express; the test below
// pins that refusal on the real store instead (gt-c3ygl). RunCreatorOptionContract
// names a creator in the request, and the tracker records the token's actor
// instead (see Client.Create); RunActorContract below pins who it records.
//
// This is integration tier because it needs a real Postgres; the integration
// sweep runs it on every landing, and the tracker's own store contract runs on
// Postgres in the same sweep. The unit tier covers the mapping and the batch
// close rules (unit_test.go).
func TestIntegrationTrackerClientSatisfiesTheClientContract(t *testing.T) {
	beadsfake.RunClientContract(t, newTrackerRoles)
}

// A seat is not the holder of an issue another actor holds, so the tracker
// refuses it a reassignment, and the holder's own seat makes the same change
// (gt-26dvk). The contract run acts through the holder for the reassign rows.
func TestIntegrationASeatCannotReassignAnotherActorsIssue(t *testing.T) {
	roles := newTrackerRoles(t)
	is := heldByAlice(t, roles)
	bob := "town/polecats/bob"
	err := roles.Seat.Update(is.ID, beads.UpdateOptions{Assignee: &bob})
	if !errors.Is(err, api.ErrGuardFailed) {
		t.Fatalf("a seat reassigning alice's held issue = %v, want api.ErrGuardFailed", err)
	}
	got, err := roles.Admin.Show(is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "town/polecats/alice" {
		t.Errorf("a refused reassign changed the assignee to %q, want town/polecats/alice", got.Assignee)
	}
}

// Delete is admin-only: a seat is refused before the store, and the issue it
// tried to delete is still there (gt-26dvk).
func TestIntegrationASeatCannotDeleteIssues(t *testing.T) {
	roles := newTrackerRoles(t)
	is := heldByAlice(t, roles)
	err := roles.Seat.DeleteIssues(is.ID)
	if !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("a seat deleting %s = %v, want api.ErrForbidden", is.ID, err)
	}
	if _, err := roles.Admin.Show(is.ID); err != nil {
		t.Errorf("the issue a seat tried to delete is gone: %v", err)
	}
}

// heldByAlice creates an issue as the seat and has alice claim it, so another
// seat is not its holder.
func heldByAlice(t *testing.T, roles beadsfake.Roles) *beads.Issue {
	t.Helper()
	is, err := roles.Seat.Create(beads.CreateOptions{Title: "held by alice", Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	alice, inProgress := "town/polecats/alice", "in_progress"
	if err := roles.Holder(alice).Update(is.ID, beads.UpdateOptions{Status: &inProgress, Assignee: &alice}); err != nil {
		t.Fatal(err)
	}
	return is
}

// RunDepListUpContract names its own subtest, so the tracker run gets no
// wrapper (as the fake and bd do). A wisp dependent it counts is the store's
// Dependents fill, so a store that drops ephemeral rows fails this case.
func TestIntegrationTrackerClientSatisfiesTheDepListUpContract(t *testing.T) {
	beadsfake.RunDepListUpContract(t, func(t *testing.T) beads.Client { return newTracker(t, "town/crew/dipper", api.ScopeSeat) })
}

// The external-target DepListUp case runs against the tracker too, so the
// store's external match and the fake's agree (gt-etvxc).
func TestIntegrationTrackerClientSatisfiesTheDepListUpExternalContract(t *testing.T) {
	beadsfake.RunDepListUpExternalContract(t, func(t *testing.T) beads.Client { return newTracker(t, "town/crew/dipper", api.ScopeSeat) })
}

// The show-text suite runs against the tracker: ShowText renders the id and
// title, and a missing id is an error.
func TestIntegrationTrackerClientShowText(t *testing.T) {
	beadsfake.RunShowTextContract(t, func(t *testing.T) beads.Client { return newTracker(t, "town/crew/dipper", api.ScopeSeat) })
}

// The tracker's list query cannot express the notes and update-time filters, so
// List refuses them rather than answer with a wider set, even when an issue
// carrying the text exists. This is the tracker's side of the contract that
// RunNotesFilterContract holds the fake and bd to (gt-c3ygl).
func TestIntegrationTrackerClientRefusesTheNotesFilters(t *testing.T) {
	c := newTracker(t, "town/crew/dipper", api.ScopeSeat)
	is, err := c.Create(beads.CreateOptions{Title: "noted", Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AppendNotes(is.ID, "MERGE REJECTION (build): red"); err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]beads.ListOptions{
		"NotesContains": {NotesContains: "merge rejection", Priority: -1},
		"UpdatedAfter":  {UpdatedAfter: time.Now().Add(-time.Hour), Priority: -1},
	} {
		if got, err := c.List(opts); !errors.Is(err, trackerclient.ErrListFilter) {
			t.Errorf("List{%s} = %d issues, %v; want an ErrListFilter refusal", name, len(got), err)
		}
	}
}

func TestIntegrationTrackerClientSatisfiesTheActorContract(t *testing.T) {
	beadsfake.RunActorContract(t, func(t *testing.T, actor string) beads.Client { return newTracker(t, actor, api.ScopeSeat) })
}

// The maintenance surface runs with an admin token (configuration writes need
// one; a seat token gets api.ErrForbidden): config, counts, stats, wisps and
// the events journal, on one database for the whole run as the contract wants.
func TestIntegrationTrackerClientSatisfiesTheAdminContract(t *testing.T) {
	beadsfake.RunAdminContract(t, func(t *testing.T) beadsfake.AdminClient { return newTracker(t, "town/daemon", api.ScopeAdmin) })
}

// A seat's token cannot write configuration; the adapter says so with the
// API's forbidden error, not as a missing issue and not silently.
func TestIntegrationASeatCannotWriteConfiguration(t *testing.T) {
	seat := newTracker(t, "town/polecats/x", api.ScopeSeat)
	err := seat.ConfigSet("types.custom", "agent")
	if !errors.Is(err, api.ErrForbidden) || errors.Is(err, beads.ErrNotFound) {
		t.Errorf("ConfigSet with a seat token = %v, want api.ErrForbidden", err)
	}
	if v, err := seat.ConfigGet("types.custom"); err != nil || v != "" {
		t.Errorf("ConfigGet after the refused write = %q, %v; want unset and no error (seats may read)", v, err)
	}
}

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

// newTracker runs the real tracker (Postgres store, HTTP server, Go client)
// in-process on a fresh migrated database and returns a beads.Client over it
// acting as actor. Integration tier: one clone of a template database, about
// 70 ms, plus a loopback server.
func newTracker(t *testing.T, actor string, scope api.Scope) *trackerclient.Client {
	t.Helper()
	pool := pgtest.New(t, "migrated")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown'), ('hq', 'town')"); err != nil {
		t.Fatal(err)
	}
	pg := store.NewPostgres(pool)
	token, err := pg.IssueTokenScoped(ctx, actor, scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Config{Store: pg, Auth: pg, Health: pg, SchemaLevel: store.SchemaLevel()}).Handler())
	t.Cleanup(srv.Close)
	tc, err := client.New(client.Config{BaseURL: srv.URL, Token: token, SchemaLevel: store.SchemaLevel()})
	if err != nil {
		t.Fatal(err)
	}
	return trackerclient.New(tc, "gt")
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
	beadsfake.RunClientContract(t, func(t *testing.T) beads.Client { return newTracker(t, "town/crew/dipper", api.ScopeSeat) })
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

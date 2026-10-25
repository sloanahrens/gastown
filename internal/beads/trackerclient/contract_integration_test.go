//go:build integration

package trackerclient_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/beads/trackerclient"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/server"
	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// newTracker runs the real tracker (Postgres store, HTTP server, Go client)
// in-process on a fresh migrated database and returns a beads.Client over it
// acting as actor. Integration tier: one clone of a template database, about
// 70 ms, plus a loopback server.
func newTracker(t *testing.T, actor string) *trackerclient.Client {
	t.Helper()
	pool := pgtest.New(t, "migrated")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown'), ('hq', 'town')"); err != nil {
		t.Fatal(err)
	}
	pg := store.NewPostgres(pool)
	token, err := pg.IssueToken(ctx, actor, nil)
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

// The adapter must behave like bd on every case the fake and bd are held to.
// The one suite it does not run is RunCreatorOptionContract: the tracker
// records the creator as the token's actor and ignores one named in a request
// (see Client.Create), and RunActorContract below pins who it records.
//
// This is integration tier because it needs a real Postgres; the integration
// sweep runs it on every landing, and the tracker's own store contract runs on
// Postgres in the same sweep. The unit tier covers the mapping and the batch
// close rules (unit_test.go).
func TestIntegrationTrackerClientSatisfiesTheClientContract(t *testing.T) {
	beadsfake.RunClientContract(t, func(t *testing.T) beads.Client { return newTracker(t, "town/crew/dipper") })
}

func TestIntegrationTrackerClientSatisfiesTheActorContract(t *testing.T) {
	beadsfake.RunActorContract(t, func(t *testing.T, actor string) beads.Client { return newTracker(t, actor) })
}

// The maintenance surface: config, counts, stats, tables, wisps, the events
// journal and export, on one database for the whole run as the contract wants.
func TestIntegrationTrackerClientSatisfiesTheAdminContract(t *testing.T) {
	beadsfake.RunAdminContract(t, func(t *testing.T) beadsfake.AdminClient { return newTracker(t, "town/crew/dipper") })
}

//go:build integration

package trackerclient_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/backend"
	"github.com/steveyegge/gastown/internal/beads/trackerclient"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/server"
	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// townAndSeat runs the real tracker (Postgres store, HTTP server) in-process on
// one fresh database and returns the two clients the calls under test come
// from. The town-side client is built the way the daemon builds it:
// backend.TownConfig reads the admin credential from the town's
// settings/daemon.env. The seat client is built from a seat token, as a seat's
// TRACKER_TOKEN is. Integration tier: a clone of a template database, plus a
// loopback server.
func townAndSeat(t *testing.T) (town, seat *trackerclient.Client) {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.New(t, "migrated")
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown'), ('hq', 'town')"); err != nil {
		t.Fatal(err)
	}
	pg := store.NewPostgres(pool)
	adminToken, err := pg.IssueTokenScoped(ctx, "town/daemon", api.ScopeAdmin, nil)
	if err != nil {
		t.Fatal(err)
	}
	seatToken, err := pg.IssueTokenScoped(ctx, "town/polecats/x", api.ScopeSeat, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Config{Store: pg, Auth: pg, Health: pg, SchemaLevel: store.SchemaLevel()}).Handler())
	t.Cleanup(srv.Close)

	townRoot := t.TempDir()
	settings := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settings, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settings, "daemon.env"), []byte("TRACKER_ADMIN_TOKEN="+adminToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TRACKER_URL": srv.URL, "TRACKER_TOKEN": seatToken}
	getenv := func(key string) string { return env[key] }

	townCfg, err := backend.TownConfig(townRoot, getenv)
	if err != nil {
		t.Fatal(err)
	}
	townTC, err := client.New(townCfg.Tracker)
	if err != nil {
		t.Fatal(err)
	}
	seatTC, err := client.New(backend.FromEnv(getenv).Tracker)
	if err != nil {
		t.Fatal(err)
	}
	return trackerclient.New(townTC, "gt"), trackerclient.New(seatTC, "gt")
}

// heldIssue files an issue the daemon records as held by another actor, the
// state a dead holder's bead is in when the landing worker or the reaper reaches
// it.
func heldIssue(t *testing.T, town *trackerclient.Client) string {
	t.Helper()
	is, err := town.Create(beads.CreateOptions{Title: "held by dipper", Assignee: "town/crew/dipper", Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	return is.ID
}

// A seat cannot clear another actor's assignee, even with Force: the holder
// check on the store refuses it, and the refusal writes nothing.
func TestIntegrationASeatCannotClearAHeldAssigneeEvenWithForce(t *testing.T) {
	town, seat := townAndSeat(t)
	id := heldIssue(t, town)
	empty := ""
	if err := seat.Update(id, beads.UpdateOptions{Assignee: &empty, Force: true}); !errors.Is(err, api.ErrGuardFailed) {
		t.Fatalf("seat forced clear = %v, want the holder refusal", err)
	}
	is, err := town.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	if is.Assignee != "town/crew/dipper" {
		t.Errorf("assignee after the refused clear = %q, want the holder kept", is.Assignee)
	}
}

// A seat cannot delete an issue: the delete route is admin-only, and the
// refusal comes before any write.
func TestIntegrationASeatCannotDeleteAnIssue(t *testing.T) {
	town, seat := townAndSeat(t)
	id := heldIssue(t, town)
	if err := seat.DeleteIssues(id); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("seat delete = %v, want the admin-only refusal", err)
	}
	if _, err := town.Show(id); err != nil {
		t.Errorf("the issue is gone after a refused delete: %v", err)
	}
}

// The town-side client, on the admin credential the daemon reads, clears a held
// assignee with Force, the write the landing rework path makes once its tracker
// verbs have a client.
func TestIntegrationTheTownsAdminCredentialClearsAHeldAssignee(t *testing.T) {
	town, _ := townAndSeat(t)
	id := heldIssue(t, town)
	empty := ""
	if err := town.Update(id, beads.UpdateOptions{Assignee: &empty, Force: true}); err != nil {
		t.Fatalf("town forced clear: %v", err)
	}
	is, err := town.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	if is.Assignee != "" {
		t.Errorf("assignee after the clear = %q, want none", is.Assignee)
	}
}

// The town-side client, on the admin credential, deletes an issue: the reaper's
// delete path.
func TestIntegrationTheTownsAdminCredentialDeletesAnIssue(t *testing.T) {
	town, _ := townAndSeat(t)
	id := heldIssue(t, town)
	if err := town.DeleteIssues(id); err != nil {
		t.Fatalf("town delete: %v", err)
	}
	if _, err := town.Show(id); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("show after the delete = %v, want beads.ErrNotFound", err)
	}
}

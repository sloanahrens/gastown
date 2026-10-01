//go:build integration

package beads_test

import (
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestIntegrationClientContract runs the Client and Admin contracts against
// bd on databases from the shared test Dolt container's pool, through the exported
// constructor. It needs GT_TEST_DOCKER=1 and Docker (run it under gt slot
// run); without them it fails rather than skips.
// clientLanes is how many databases the client cases share, one case per
// database at a time.
const clientLanes = 4

func TestIntegrationClientContract(t *testing.T) {
	if !testutil.DockerTestsEnabled() {
		t.Fatal("TestIntegrationClientContract needs the Dolt test container: run under gt slot run with GT_TEST_DOCKER=1")
	}
	if err := testutil.EnsureDoltContainerForTestMain(); err != nil {
		t.Fatalf("Dolt test container: %v", err)
	}
	port, err := strconv.Atoi(testutil.DoltContainerPort())
	if err != nil || port == 0 {
		t.Fatalf("no Dolt test container (port %q): %v", testutil.DoltContainerPort(), err)
	}
	// The client cases run in parallel, but two of them writing one database
	// at once fail bd's dolt commit with "Error 1213 (40001): serialization
	// failure" (gt-6ox58.2, be-321), and a database per case costs a bd init
	// each, which the single-core server runs one after another (27 inits:
	// 2 min). So they share clientLanes databases, each held by one case at a
	// time (each asserts only on its own issues). The admin cases run in
	// turn, each on its own.
	//
	// bd reads beads.role from git config and, when it is unset, prints a
	// warning (GH#2950) ahead of the JSON on every create. The isolated
	// client runs without HOME and a t.TempDir() is no git repo, so set the
	// role through git's environment config, which the bd subprocesses
	// inherit. Set here, before the parallel subtests start.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "beads.role")
	t.Setenv("GIT_CONFIG_VALUE_0", "maintainer")
	newDB := func(t *testing.T, dir string) *beads.Beads {
		b := beads.NewIsolatedWithPort(dir, port)
		if err := b.Init("gt"); err != nil {
			t.Fatalf("bd init on the test container: %v", err)
		}
		// Every store gastown makes turns the events journal on in its
		// config (gt-7iwy0.7); the admin contract reads the journal.
		if _, err := beads.EnsureEventsJournal(b); err != nil {
			t.Fatalf("events journal on: %v", err)
		}
		return b
	}
	t.Run("client", func(t *testing.T) {
		t.Parallel()
		// A lane's directory is the client test's, so its pooled database
		// outlives the case that initialised it. A lane without a db is not
		// yet initialised (or its init failed); the next case to take it
		// inits.
		type lane struct {
			dir string
			db  *beads.Beads
		}
		lanes := make(chan *lane, clientLanes)
		for range clientLanes {
			lanes <- &lane{dir: t.TempDir()}
		}
		beadsfake.RunClientContract(t, func(t *testing.T) beads.Client {
			l := <-lanes
			t.Cleanup(func() { lanes <- l })
			if l.db == nil {
				l.db = newDB(t, l.dir)
			}
			return l.db
		})
	})
	t.Run("actor", func(t *testing.T) {
		t.Parallel()
		// The isolated client strips the inherited BD_ACTOR, so the actor
		// bd records can only come from ActingAs (gt-0wkug).
		beadsfake.RunActorContract(t, func(t *testing.T, actor string) beads.Client {
			return newDB(t, t.TempDir()).ActingAs(actor)
		})
	})
	t.Run("admin", func(t *testing.T) {
		t.Parallel()
		beadsfake.RunAdminContract(t, func(t *testing.T) beadsfake.AdminClient { return newDB(t, t.TempDir()) })
	})
}

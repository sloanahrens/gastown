package beads

import (
	"testing"
)

// An isolated client must not switch off bd's test-database firewall
// (BEADS_TEST_SERVER=1) just because it has a port: a test that picked up
// GT_DOLT_PORT=3307 from the operator's shell then minted testdb_* databases
// on production (gt-fcxe9.9, deep review B2-03). Only a port testutil
// registered for a container it started, and never 3307, gets it, and the
// same holds for the remote-migrate escape hatch (deep review B2-02).
//
// Not parallel: t.Setenv, and the registry is process-wide.
func TestIsolatedClientTestServerEnv(t *testing.T) {
	t.Setenv("BEADS_TEST_SERVER", "1") // the test process itself may carry it

	const registered, unregistered = 19998, 19999
	RegisterTestServerPort(registered)
	RegisterTestServerPort(productionDoltPort) // ignored
	t.Cleanup(func() { unregisterTestServerPort(registered) })

	for _, tc := range []struct {
		name string
		port int
		want int
	}{
		{"unregistered port", unregistered, 0},
		{"production port even if registered", productionDoltPort, 0},
		{"registered test container", registered, 1},
	} {
		b := NewIsolatedWithPort(t.TempDir(), tc.port)
		for _, env := range []struct {
			name string
			got  []string
		}{
			{"run", b.buildRunEnv()},
			{"routing", b.buildRoutingEnv()},
		} {
			if got := countEnvPrefix(env.got, "BEADS_TEST_SERVER="); got != tc.want {
				t.Errorf("%s, %s env: BEADS_TEST_SERVER count = %d, want %d", tc.name, env.name, got, tc.want)
			}
			if got := countEnvPrefix(env.got, allowRemoteMigrateEnv+"="); got != tc.want {
				t.Errorf("%s, %s env: %s count = %d, want %d", tc.name, env.name, allowRemoteMigrateEnv, got, tc.want)
			}
			if tc.want == 1 && !containsEnv(env.got, "BEADS_TEST_SERVER=1") {
				t.Errorf("%s, %s env: want BEADS_TEST_SERVER=1", tc.name, env.name)
			}
		}
	}

	// A plain isolated client (no port) never carries it either.
	if got := countEnvPrefix(NewIsolated(t.TempDir()).buildRunEnv(), "BEADS_TEST_SERVER="); got != 0 {
		t.Errorf("portless isolated client: BEADS_TEST_SERVER count = %d, want 0", got)
	}
}

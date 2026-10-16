package beads

import (
	"path/filepath"
	"strings"
	"testing"
)

// The new-database opt-in rides bd init and nothing else: creating a database
// is the one thing it authorizes, so a read verb that carried it would be
// gt saying yes to a create nobody asked for (be-d4v).
func assertOptInOnlyOnInits(t *testing.T, calls []recordedCall) {
	t.Helper()
	inits, others := 0, 0
	for _, c := range calls {
		got, ok := lastEnvValue(c.env, allowNewDatabaseEnv)
		argv := strings.Join(c.args, " ")
		if c.args[0] == "init" {
			inits++
			if !ok || got != "1" {
				t.Errorf("bd %s: %s = %q, want 1 (env %v)", argv, allowNewDatabaseEnv, got, c.env)
			}
			continue
		}
		others++
		if ok {
			t.Errorf("bd %s carried %s=%q, want it only on bd init", argv, allowNewDatabaseEnv, got)
		}
	}
	if inits == 0 || others == 0 {
		t.Fatalf("test covered %d init and %d other calls, want both", inits, others)
	}
}

// Init and InitDatabase are the two ways a pinned wrapper reaches bd init, and
// gt rig add reaches both (be-d4v).
func TestInitAndInitDatabaseCarryTheNewDatabaseOptIn(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	b := newRecordedBeads(t.TempDir(), r)

	if err := b.Init("gt"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.InitDatabase(InitOptions{Prefix: "gt", Database: "zzrig", ServerPort: 3307}); err != nil {
		t.Fatalf("InitDatabase: %v", err)
	}
	if _, err := b.Run("status"); err != nil {
		t.Fatalf("bd status: %v", err)
	}
	if _, err := b.StatsJSON(); err != nil {
		t.Fatalf("StatsJSON: %v", err)
	}

	assertOptInOnlyOnInits(t, r.calls())
}

// gt rig add and gt install both initialize a database through a plain
// wrapper, whose environment gt supplies itself, so the opt-in is added on
// that path too (be-d4v).
func TestPlainInitDatabaseCarriesTheNewDatabaseOptIn(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	b := newPlainRecorded(t, r)

	if err := b.InitDatabase(InitOptions{Prefix: "hq", Database: "hq", ServerPort: 3307}); err != nil {
		t.Fatalf("InitDatabase: %v", err)
	}
	if err := b.MigrateRepoID(); err != nil {
		t.Fatalf("MigrateRepoID: %v", err)
	}

	assertOptInOnlyOnInits(t, r.calls())
}

// ensureDatabaseInitialized builds its bd init argv itself, outside the
// wrapper, and repairs a workspace bd left half-written — the one gt-driven
// init no wrapper env reaches (be-d4v).
func TestEnsureDatabaseInitializedCarriesTheNewDatabaseOptIn(t *testing.T) {
	townRoot := newFakeTown(t)
	beadsDir := filepath.Join(townRoot, "zzrig", ".beads")
	writeMetadata(t, beadsDir, map[string]any{
		"backend": "dolt", "dolt_mode": "server", "dolt_database": "zzrig",
		"dolt_server_host": "127.0.0.1", "dolt_server_port": 1,
	})

	logPath := mockBDInitEnvRecorder(t, "")
	if err := ensureDatabaseInitialized(beadsDir); err != nil {
		t.Fatalf("ensureDatabaseInitialized: %v", err)
	}

	got := mockBDLogOptInByArgv(t, readMockBDLog(t, logPath))
	if len(got) < 2 {
		t.Fatalf("mock bd saw %d calls, want the init and the verbs beside it: %v", len(got), got)
	}
	initArgv := "init --prefix gt --database zzrig --server"
	if got[initArgv] != "1" {
		t.Errorf("bd %s carried %s=%q, want 1", initArgv, allowNewDatabaseEnv, got[initArgv])
	}
	for argv, val := range got {
		if argv != initArgv && val != "<unset>" {
			t.Errorf("bd %s carried %s=%q, want it only on bd init", argv, allowNewDatabaseEnv, val)
		}
	}
}

// mockBDLogOptInByArgv reads the mock bd's log into one entry per call: the
// argv, and the new-database value that call's environment carried.
func mockBDLogOptInByArgv(t *testing.T, log string) map[string]string {
	t.Helper()
	got := map[string]string{}
	argv := ""
	for _, line := range strings.Split(log, "\n") {
		switch {
		case strings.HasPrefix(line, "argv "):
			argv = strings.TrimPrefix(line, "argv ")
		case strings.HasPrefix(line, newDatabaseLogKey+" "):
			got[argv] = strings.TrimPrefix(line, newDatabaseLogKey+" ")
		}
	}
	return got
}

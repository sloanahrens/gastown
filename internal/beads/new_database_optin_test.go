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

// The opt-in is one entry with one value whatever the process inherited:
// appended beside an inherited entry it is two, and bd then reads whichever of
// them its getenv reaches first (gt-860yi).
func TestApplyNewDatabaseOptInReplacesAnInheritedValue(t *testing.T) {
	t.Parallel()
	got := applyNewDatabaseOptIn([]string{"PATH=/bin", allowNewDatabaseEnv + "=0"}, []string{"init", "--prefix", "gt"})

	count := 0
	for _, entry := range got {
		if !strings.HasPrefix(entry, allowNewDatabaseEnv+"=") {
			continue
		}
		count++
		if entry != allowNewDatabaseEnv+"=1" {
			t.Errorf("entry = %q, want %s=1", entry, allowNewDatabaseEnv)
		}
	}
	if count != 1 {
		t.Errorf("%s appears %d times, want 1 (env=%v)", allowNewDatabaseEnv, count, got)
	}
}

// Growing the env must not write into the caller's array: the caller still
// holds its slice, and a spare slot it did not know it owned would carry the
// opt-in (gt-860yi).
func TestApplyNewDatabaseOptInLeavesTheCallersArrayAlone(t *testing.T) {
	t.Parallel()
	backing := make([]string, 2, 3)
	backing[0], backing[1] = "PATH=/bin", "BD_MACHINE=1"

	applyNewDatabaseOptIn(backing[:2], []string{"init"})

	// The third slot is the caller's spare capacity: the append an aliasing
	// implementation would make lands there.
	if spare := backing[:cap(backing)][2]; spare != "" {
		t.Errorf("the append wrote %q into the caller's spare capacity, want the caller's array untouched", spare)
	}
}

// Init and InitDatabase are the two ways a pinned wrapper reaches bd init, and
// gt rig add reaches both (be-d4v).
func TestInitAndInitDatabaseCarryTheNewDatabaseOptIn(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	b := newRecordedBeads(t.TempDir(), r)

	if err := InitForTest(b, "gt"); err != nil {
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

	// The same assertion the wrapper paths use, so the init is found by its
	// command word rather than by the argv this function happens to build.
	assertOptInOnlyOnInits(t, mockBDInitCalls(t, readMockBDLog(t, logPath)))
}

// mockBDInitCalls reads the mock bd's log into the recordedCall shape
// assertOptInOnlyOnInits asserts on: one call per argv line, carrying the
// new-database value that call's environment carried.
func mockBDInitCalls(t *testing.T, log string) []recordedCall {
	t.Helper()
	var calls []recordedCall
	for _, line := range strings.Split(log, "\n") {
		if argv, ok := strings.CutPrefix(line, "argv "); ok {
			args := strings.Fields(argv)
			if len(args) == 0 {
				t.Fatalf("mock bd logged an empty argv: %q", log)
			}
			calls = append(calls, recordedCall{args: args})
			continue
		}
		val, ok := strings.CutPrefix(line, newDatabaseLogKey+" ")
		if !ok {
			continue
		}
		if len(calls) == 0 {
			t.Fatalf("mock bd logged %s before any argv: %q", newDatabaseLogKey, log)
		}
		if val != mockBDUnsetEnv {
			calls[len(calls)-1].env = []string{allowNewDatabaseEnv + "=" + val}
		}
	}
	return calls
}

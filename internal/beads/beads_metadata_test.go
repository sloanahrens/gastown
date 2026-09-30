package beads

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMetadata writes a metadata.json carrying fields into beadsDir.
func writeMetadata(t *testing.T, beadsDir string, fields map[string]any) {
	t.Helper()
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", beadsDir, err)
	}
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), data, 0o600); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
}

func readMetadata(t *testing.T, beadsDir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	return fields
}

// TestInitDatabaseTargetRefusesAnUnnamedWorkspace pins the fail-closed rule of
// gt-170zk: gt names the database a bd init is about to create, or it does not
// run one.
func TestInitDatabaseTargetRefusesAnUnnamedWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
	}{
		{"no metadata.json", nil},
		{"metadata.json without dolt_database", map[string]any{
			"backend": "dolt", "database": "dolt", "dolt_mode": "server",
		}},
		{"metadata.json with an empty dolt_database", map[string]any{
			"backend": "dolt", "dolt_mode": "server", "dolt_database": "",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beadsDir := filepath.Join(t.TempDir(), "rig", ".beads")
			if tc.fields != nil {
				writeMetadata(t, beadsDir, tc.fields)
			} else if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatal(err)
			}

			db, err := InitDatabaseTarget(beadsDir)
			if !errors.Is(err, ErrNoConfiguredDatabase) {
				t.Fatalf("InitDatabaseTarget(%s) = (%q, %v), want ErrNoConfiguredDatabase", beadsDir, db, err)
			}
		})
	}
}

// TestInitDatabaseTargetReadsTheConfiguredName covers the workspace shape gt
// actually initializes: metadata.json that names the database the rig was
// provisioned with (the rig-add path writes it before this runs).
func TestInitDatabaseTargetReadsTheConfiguredName(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), "rig", ".beads")
	writeMetadata(t, beadsDir, map[string]any{"dolt_mode": "server", "dolt_database": "zzrig"})

	db, err := InitDatabaseTarget(beadsDir)
	if err != nil {
		t.Fatalf("InitDatabaseTarget: %v", err)
	}
	if db != "zzrig" {
		t.Errorf("InitDatabaseTarget = %q, want %q", db, "zzrig")
	}
}

// TestInitDatabaseTargetReadsLegacyConfigJSON keeps pre-metadata workspaces
// initializable: bd still reads the config.json field it migrates from.
func TestInitDatabaseTargetReadsLegacyConfigJSON(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), "rig", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.json"),
		[]byte(`{"dolt_database":"zzlegacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := InitDatabaseTarget(beadsDir)
	if err != nil {
		t.Fatalf("InitDatabaseTarget: %v", err)
	}
	if db != "zzlegacy" {
		t.Errorf("InitDatabaseTarget = %q, want %q", db, "zzlegacy")
	}
}

// TestEnsureMetadataDatabaseNamesTheDatabase is the second half of the fix:
// bd init --server is documented to write dolt_mode=server WITHOUT
// dolt_database, which leaves the workspace naming no database at all, and
// every later bd open in it falls back to the default "beads".
func TestEnsureMetadataDatabaseNamesTheDatabase(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), "rig", ".beads")
	writeMetadata(t, beadsDir, map[string]any{
		"backend": "dolt", "database": "dolt", "dolt_mode": "server",
		"dolt_server_host": "127.0.0.1", "dolt_server_port": 3307,
		"project_id": "11111111-2222-3333-4444-555555555555",
	})

	if err := EnsureMetadataDatabase(beadsDir, "zzrig"); err != nil {
		t.Fatalf("EnsureMetadataDatabase: %v", err)
	}

	fields := readMetadata(t, beadsDir)
	if fields["dolt_database"] != "zzrig" {
		t.Errorf("dolt_database = %v, want zzrig", fields["dolt_database"])
	}
	// Every other field survives: metadata.json is git-tracked, and a rewrite
	// that dropped the server config or identity would break the workspace it
	// was meant to repair.
	for key, want := range map[string]any{
		"backend":          "dolt",
		"database":         "dolt",
		"dolt_mode":        "server",
		"dolt_server_host": "127.0.0.1",
		"project_id":       "11111111-2222-3333-4444-555555555555",
	} {
		if fields[key] != want {
			t.Errorf("field %s = %v, want %v", key, fields[key], want)
		}
	}
	// InitDatabaseTarget now resolves, where before the repair it refused.
	db, err := InitDatabaseTarget(beadsDir)
	if err != nil || db != "zzrig" {
		t.Errorf("InitDatabaseTarget after repair = (%q, %v), want (zzrig, nil)", db, err)
	}
}

// TestEnsureMetadataDatabaseCreatesMissingFile covers the fresh workspace: gt
// records the database it initialized so the workspace starts out named.
func TestEnsureMetadataDatabaseCreatesMissingFile(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), "rig", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := EnsureMetadataDatabase(beadsDir, "zzrig"); err != nil {
		t.Fatalf("EnsureMetadataDatabase: %v", err)
	}
	if got := readMetadata(t, beadsDir)["dolt_database"]; got != "zzrig" {
		t.Errorf("dolt_database = %v, want zzrig", got)
	}
}

// TestEnsureMetadataDatabasePreservesMode pins that a repair does not chmod a
// git-tracked metadata.json, which would show up as a mode change in the rig
// worktree.
func TestEnsureMetadataDatabasePreservesMode(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), "rig", ".beads")
	writeMetadata(t, beadsDir, map[string]any{"dolt_mode": "server"})
	path := filepath.Join(beadsDir, "metadata.json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureMetadataDatabase(beadsDir, "zzrig"); err != nil {
		t.Fatalf("EnsureMetadataDatabase: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("metadata.json mode = %o, want 644", perm)
	}
}

// TestEnsureMetadataDatabaseRefusesToClobberUnparseableMetadata: a
// present-but-corrupt metadata.json is a config problem, and rewriting it
// would discard whatever the operator or a newer bd put there.
func TestEnsureMetadataDatabaseRefusesToClobberUnparseableMetadata(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), "rig", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(beadsDir, "metadata.json")
	const corrupt = "{not json"
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureMetadataDatabase(beadsDir, "zzrig"); err == nil {
		t.Fatal("EnsureMetadataDatabase overwrote an unparseable metadata.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != corrupt {
		t.Errorf("metadata.json was rewritten: %q", data)
	}
}

// TestEnsureMetadataDatabaseRejectsEmptyName keeps the repair from writing a
// database-less metadata.json in the name of naming one.
func TestEnsureMetadataDatabaseRejectsEmptyName(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), "rig", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMetadataDatabase(beadsDir, ""); err == nil {
		t.Fatal("EnsureMetadataDatabase accepted an empty database name")
	}
}

// TestWithDatabaseTargetReplacesInheritedSelector pins the env half: the
// subprocess carries exactly the database gt chose, whatever the calling
// process happened to have inherited.
func TestWithDatabaseTargetReplacesInheritedSelector(t *testing.T) {
	env := withDatabaseTarget([]string{"PATH=/bin", "BEADS_DOLT_SERVER_DATABASE=someoneelse"}, "zzrig")

	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_DATABASE=") {
			count++
			if entry != "BEADS_DOLT_SERVER_DATABASE=zzrig" {
				t.Errorf("entry = %q, want BEADS_DOLT_SERVER_DATABASE=zzrig", entry)
			}
		}
	}
	if count != 1 {
		t.Errorf("BEADS_DOLT_SERVER_DATABASE appears %d times, want 1 (env=%v)", count, env)
	}
}

// mockBDInitEnvRecorder puts a bd on PATH that logs each call's argv and the
// database the child was told to use, then succeeds. The argv-only recorder in
// beads_types_test.go cannot show the env propagation this fix is about.
// script, when non-empty, replaces the whole body (for mocks that must also
// touch the workspace).
func mockBDInitEnvRecorder(t *testing.T, script string) (logPath string) {
	t.Helper()
	binDir := t.TempDir()
	logPath = filepath.Join(binDir, "bd.log")
	if script == "" {
		script = `exit 0`
	}
	body := `#!/bin/sh
{
  printf 'argv %s\n' "$*"
  printf 'database %s\n' "${BEADS_DOLT_SERVER_DATABASE-<unset>}"
} >> "` + logPath + `"
` + script + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(body), 0o755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// newFakeTown builds the minimal town a beads directory resolves against.
func newFakeTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"type":"town"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// TestEnsureDatabaseInitializedNamesTheDatabaseItCreates pins the end-to-end
// invariant of the fix: when gt runs bd init, the argv names the database (so
// bd cannot fall back to its default "beads") and the child env carries it too.
func TestEnsureDatabaseInitializedNamesTheDatabaseItCreates(t *testing.T) {
	townRoot := newFakeTown(t)

	// Server-mode metadata naming the database, with no .dolt-data/<name>
	// directory: the fresh-clone case ensureDatabaseInitialized exists for.
	beadsDir := filepath.Join(townRoot, "zzrig", ".beads")
	writeMetadata(t, beadsDir, map[string]any{
		"backend": "dolt", "dolt_mode": "server", "dolt_database": "zzrig",
		"dolt_server_host": "127.0.0.1", "dolt_server_port": 1,
	})

	logPath := mockBDInitEnvRecorder(t, "")

	if err := ensureDatabaseInitialized(beadsDir); err != nil {
		t.Fatalf("ensureDatabaseInitialized: %v", err)
	}

	log := readMockBDLog(t, logPath)
	if !strings.Contains(log, "argv init --prefix gt --database zzrig --server") {
		t.Errorf("bd init argv did not name the database:\n%s", log)
	}
	if !strings.Contains(log, "database zzrig") {
		t.Errorf("bd init env did not carry the database name:\n%s", log)
	}
}

// TestEnsureDatabaseInitializedRepairsMetadataBDLeftUnnamed covers the state
// that reproduced the orphan: bd init leaves the workspace naming no database,
// where every later bd open falls back to the default "beads" (gt-170zk).
func TestEnsureDatabaseInitializedRepairsMetadataBDLeftUnnamed(t *testing.T) {
	townRoot := newFakeTown(t)
	beadsDir := filepath.Join(townRoot, "zzrig", ".beads")
	writeMetadata(t, beadsDir, map[string]any{
		"backend": "dolt", "dolt_mode": "server", "dolt_database": "zzrig",
	})

	// A mock bd that behaves the way init is documented to: it rewrites
	// metadata.json without dolt_database.
	mock := `if [ "$1" = "init" ]; then
  printf '{"backend":"dolt","database":"dolt","dolt_mode":"server"}' > "$BEADS_DIR/metadata.json"
fi
exit 0`
	logPath := mockBDInitEnvRecorder(t, mock)

	if err := ensureDatabaseInitialized(beadsDir); err != nil {
		t.Fatalf("ensureDatabaseInitialized: %v", err)
	}
	if log := readMockBDLog(t, logPath); !strings.Contains(log, "argv init") {
		t.Fatalf("bd init never ran:\n%s", log)
	}

	if got := readMetadata(t, beadsDir)["dolt_database"]; got != "zzrig" {
		t.Errorf("dolt_database = %v after init, want zzrig — the workspace names no database, so bd will fall back to \"beads\"", got)
	}
}

// TestEnsureDatabaseInitializedRefusesAnUnnamedWorkspace is the regression
// guard for gt-170zk: a workspace that names no database is never handed to
// bd init, because bd's fallback would create the default database "beads" on
// whatever server the environment points at.
func TestEnsureDatabaseInitializedRefusesAnUnnamedWorkspace(t *testing.T) {
	townRoot := newFakeTown(t)
	beadsDir := filepath.Join(townRoot, "zzrig", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	logPath := mockBDInitEnvRecorder(t, "")

	err := ensureDatabaseInitialized(beadsDir)
	if !errors.Is(err, ErrNoConfiguredDatabase) {
		t.Fatalf("ensureDatabaseInitialized = %v, want ErrNoConfiguredDatabase", err)
	}
	if log := readMockBDLog(t, logPath); log != "" {
		t.Errorf("bd ran against an unnamed workspace (it would create the default \"beads\" database):\n%s", log)
	}
}

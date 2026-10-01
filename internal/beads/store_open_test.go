package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A .beads that names no database must not open: beadsdk.OpenFromConfig would
// fall back to the database "beads" and CREATE it on whatever server the
// environment points at, which is how an empty "beads" database appeared on the
// town's production server and on the shared test container (gt-22hdp.21).
// The refusal comes before any connection, so no server is needed to see it.
func TestOpenStoreFromConfig_RefusesBeadsDirWithoutDatabase(t *testing.T) {
	t.Parallel()
	for name, metadata := range map[string]string{
		"no metadata.json":        "",
		"metadata without a name": `{"backend":"dolt","dolt_mode":"server"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			beadsDir := filepath.Join(t.TempDir(), ".beads")
			if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if metadata != "" {
				if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			store, err := OpenStoreFromConfig(context.Background(), beadsDir)
			if err == nil {
				_ = store.Close()
			}
			if !errors.Is(err, ErrNoConfiguredDatabase) {
				t.Fatalf("OpenStoreFromConfig(%s) = %v, want ErrNoConfiguredDatabase", beadsDir, err)
			}
		})
	}
}

// A server-mode workspace whose database the town's Dolt does not have must not
// open either: the library's open carries CreateIfMissing true, so it would
// CREATE the database at v1.0.5's schema 49, after which the fork bd refuses
// every write in that rig (deep review B2-26 rank 1). The refusal is a stat of
// the town's data directory, so no server is needed to see it.
func TestOpenStoreFromConfig_RefusesDatabaseTheTownDoesNotHave(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStoreFromConfig(context.Background(), beadsDir)
	if err == nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrDatabaseMissing) {
		t.Fatalf("OpenStoreFromConfig(%s) = %v, want ErrDatabaseMissing", beadsDir, err)
	}

	// The gate is the database, not the workspace: the town holding it passes
	// even though no store is opened here (the library open would need a live
	// server, and the gate has already run by then).
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data", "hq"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := requireExistingDatabase(beadsDir, "hq"); err != nil {
		t.Fatalf("requireExistingDatabase(hq) with the database present = %v, want nil", err)
	}
}

// A workspace the gate cannot place — embedded mode, no town above it, a town
// whose Dolt is on another host — is left to the library.
func TestRequireExistingDatabase_LeavesWhatItCannotSee(t *testing.T) {
	t.Parallel()

	embedded := t.TempDir()
	if err := os.WriteFile(filepath.Join(embedded, "metadata.json"), []byte(`{"dolt_mode":"embedded","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := requireExistingDatabase(embedded, "hq"); err != nil {
		t.Errorf("embedded mode = %v, want nil", err)
	}

	untowned := t.TempDir()
	if err := os.WriteFile(filepath.Join(untowned, "metadata.json"), []byte(`{"dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := requireExistingDatabase(untowned, "hq"); err != nil {
		t.Errorf("workspace with no town = %v, want nil", err)
	}

	remote := t.TempDir()
	if err := os.MkdirAll(filepath.Join(remote, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	doltConfig := "listener:\n  host: dolt.example.com\n  port: 3307\n"
	if err := os.MkdirAll(filepath.Join(remote, ".dolt-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, ".dolt-data", "config.yaml"), []byte(doltConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(remote, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`
	if err := os.WriteFile(filepath.Join(remote, ".beads", "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := requireExistingDatabase(filepath.Join(remote, ".beads"), "hq"); err != nil {
		t.Errorf("town whose Dolt is on another host = %v, want nil", err)
	}
}

// OpenStore is the same open behind the wrapper, and refuses the same way.
func TestOpenStore_RefusesBeadsDirWithoutDatabase(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := New(workDir).OpenStore(context.Background())
	if !errors.Is(err, ErrNoConfiguredDatabase) {
		t.Fatalf("OpenStore on an unconfigured .beads = %v, want ErrNoConfiguredDatabase", err)
	}
}

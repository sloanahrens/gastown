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
		"no metadata.json":         "",
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

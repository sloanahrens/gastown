package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	beadsdk "github.com/steveyegge/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// ErrNoConfiguredDatabase is OpenStoreFromConfig's refusal of a .beads
// directory that names no Dolt database.
var ErrNoConfiguredDatabase = errors.New("beads directory names no Dolt database")

// OpenStoreFromConfig opens the in-process store for the database beadsDir's
// metadata.json names (or BEADS_DOLT_SERVER_DATABASE, which the SDK prefers).
//
// It refuses a beadsDir that names none. beadsdk.OpenFromConfig would fall back
// to the database "beads" and CREATE it on whatever server the environment
// points at: an uninitialized .beads became a stray empty "beads" database on
// the town's production server, and on the shared test container a catalog
// change mid-run (gt-22hdp.21). Every caller has a fallback for a store that
// will not open, so the refusal costs nothing a real workspace needs.
//
// beadsdk.Open is never the right call for a workspace: it ignores
// metadata.json entirely and always takes that fallback.
func OpenStoreFromConfig(ctx context.Context, beadsDir string) (beadsdk.Storage, error) {
	if os.Getenv("BEADS_DOLT_SERVER_DATABASE") == "" && configuredDatabase(beadsDir) == "" {
		return nil, fmt.Errorf("%w: %s (no dolt_database in metadata.json)", ErrNoConfiguredDatabase, beadsDir)
	}
	return beadsdk.OpenFromConfig(ctx, beadsDir)
}

// configuredDatabase reads dolt_database from the files beadsdk.OpenFromConfig
// reads, in its order: beadsDir's own metadata.json (no redirect followed),
// else the legacy config.json it migrates from.
func configuredDatabase(beadsDir string) string {
	for _, name := range []string{agentconfig.BeadsMetadataFile, "config.json"} {
		if db, present := agentconfig.BeadsFileDatabase(filepath.Join(beadsDir, name)); present {
			return db
		}
	}
	return ""
}

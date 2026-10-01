package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	beadsdk "github.com/steveyegge/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// ErrNoConfiguredDatabase is OpenStoreFromConfig's refusal of a .beads
// directory that names no Dolt database.
var ErrNoConfiguredDatabase = errors.New("beads directory names no Dolt database")

// ErrDatabaseMissing is OpenStoreFromConfig's refusal of a beadsDir whose
// database the town's Dolt server does not have.
var ErrDatabaseMissing = errors.New("beads directory names a Dolt database the server does not have")

// OpenStoreFromConfig opens the in-process store for the database beadsDir's
// metadata.json names (or BEADS_DOLT_SERVER_DATABASE, which the SDK prefers),
// read-only.
//
// It refuses a beadsDir that names none. beadsdk.OpenFromConfig would fall back
// to the database "beads" and CREATE it on whatever server the environment
// points at: an uninitialized .beads became a stray empty "beads" database on
// the town's production server, and on the shared test container a catalog
// change mid-run (gt-22hdp.21). Every caller has a fallback for a store that
// will not open, so the refusal costs nothing a real workspace needs.
//
// It also refuses a database the town's Dolt server does not already hold: the
// library's open carries CreateIfMissing true, so opening a store for an
// absent database CREATES it, born by v1.0.5's own migrations at schema 49,
// after which the fork bd refuses every write in that rig until bd migrate
// --force (gt-fcxe9.11).
//
// The store it returns is read-only (readOnlyStore): gastown writes to beads
// through bd.
//
// beadsdk.Open is never the right call for a workspace: it ignores
// metadata.json entirely and always takes that fallback.
func OpenStoreFromConfig(ctx context.Context, beadsDir string) (beadsdk.Storage, error) {
	database := os.Getenv("BEADS_DOLT_SERVER_DATABASE")
	if database == "" {
		database = configuredDatabase(beadsDir)
	}
	if database == "" {
		return nil, fmt.Errorf("%w: %s (no dolt_database in metadata.json)", ErrNoConfiguredDatabase, beadsDir)
	}
	if err := requireExistingDatabase(beadsDir, database); err != nil {
		return nil, err
	}
	store, err := beadsdk.OpenFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, err
	}
	return readOnlyStore{Storage: store}, nil
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

// requireExistingDatabase refuses database when the town's own Dolt server
// holds no such database, which is what stops the open from creating it.
//
// The test is the town's data directory, the way hasBeadsDatabase answers the
// same question for the dispatch board (internal/cmd/daemon_dispatch.go): a
// server-mode workspace names a database the server keeps as .dolt-data/<db>,
// and a database the server does not hold is an empty rig, not a store to
// open. A filesystem test on purpose — a store open must not need a working
// SQL connection to decide, and a database on disk the server has not
// registered yet fails the connect that follows.
//
// Anything the check cannot see is left to the library: a workspace with no
// server mode (or no town above it), and a town whose Dolt is on another host,
// where gastown cannot say what that server holds.
func requireExistingDatabase(beadsDir, database string) error {
	if doltMode(beadsDir) != "server" {
		return nil
	}
	townRoot := FindTownRoot(beadsDir)
	if townRoot == "" {
		return nil
	}
	if host := agentconfig.ResolveDoltHost(townRoot); host != "" && !isLocalDoltHost(host) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".dolt-data", database)); !os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("%w: %s names database %q, which %s/.dolt-data does not hold; opening it would create a database v1.0.5 births at schema 49 (run bd migrate, or gt dolt init-rig for a new rig)",
		ErrDatabaseMissing, beadsDir, database, townRoot)
}

// doltMode is the dolt_mode of beadsDir's metadata.json ("" when the file is
// absent, does not parse, or sets none). Only "server" sends gastown's store
// opens at a shared Dolt server; anything else is the library's embedded mode,
// where the database lives in the workspace itself.
func doltMode(beadsDir string) string {
	data, err := os.ReadFile(filepath.Join(beadsDir, agentconfig.BeadsMetadataFile)) //nolint:gosec // G304: bd config file of a beads directory gastown chose
	if err != nil {
		return ""
	}
	var meta struct {
		DoltMode string `json:"dolt_mode"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	return strings.TrimSpace(meta.DoltMode)
}

// isLocalDoltHost reports whether host names the machine running gastown: ""
// (what the town config carries for localhost), 127.0.0.1, ::1 or localhost.
func isLocalDoltHost(host string) bool {
	switch strings.ToLower(host) {
	case "", "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

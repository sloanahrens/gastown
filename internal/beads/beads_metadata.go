// Package beads: naming the Dolt database a gt-driven `bd init` targets, and
// recording it in the workspace afterwards.
package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// InitDatabaseTarget returns the Dolt database a gt-driven `bd init` must name
// for beadsDir, or ErrNoConfiguredDatabase when the workspace names none.
//
// Refusing is what keeps bd from naming the database itself: bd's last resort
// is its built-in default "beads", which it CREATES on whatever server the
// environment points at (gt-170zk). A directory that names no database is also
// not one gt may guess for — the rig-add and install paths record the name
// before this runs, and a guess here would mint a second database for a rig
// that already has one.
func InitDatabaseTarget(beadsDir string) (string, error) {
	if db := configuredDatabase(beadsDir); db != "" {
		return db, nil
	}
	return "", fmt.Errorf("%w: %s — refusing to run bd init, which would create its default database", ErrNoConfiguredDatabase, beadsDir)
}

// EnsureMetadataDatabase records dbName as beadsDir's dolt_database, preserving
// every other field and the file's mode.
//
// bd init --server writes dolt_mode=server without dolt_database, and a
// workspace naming no database is one where every later bd open falls back to
// the default "beads" (gt-170zk).
func EnsureMetadataDatabase(beadsDir, dbName string) error {
	if beadsDir == "" {
		return fmt.Errorf("empty beads directory")
	}
	if dbName == "" {
		return fmt.Errorf("empty database name")
	}

	path := filepath.Join(beadsDir, "metadata.json")
	fields := make(map[string]any)
	perm := os.FileMode(0o600)
	switch data, err := os.ReadFile(path); {
	case err == nil:
		if err := json.Unmarshal(data, &fields); err != nil {
			// A present-but-unparseable metadata.json is a config problem, not
			// one to paper over: rewriting it would discard whatever the
			// operator (or a newer bd) put there.
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		if info, statErr := os.Stat(path); statErr == nil {
			perm = info.Mode().Perm()
		}
	case os.IsNotExist(err):
		// Fresh workspace: write a metadata.json that names the database.
	default:
		return fmt.Errorf("reading %s: %w", path, err)
	}

	if existing, _ := fields["dolt_database"].(string); existing == dbName {
		return nil
	}
	fields["dolt_database"] = dbName

	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", path, err)
	}
	if err := atomicfile.WriteFile(path, append(data, '\n'), perm); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// withDatabaseTarget returns env with the Dolt database named, so a bd
// subprocess cannot fall back to its built-in default database. Inherited
// selectors are stripped first, matching the env builders in database.go.
func withDatabaseTarget(env []string, dbName string) []string {
	return append(StripEnvKey(env, "BEADS_DOLT_SERVER_DATABASE"), "BEADS_DOLT_SERVER_DATABASE="+dbName)
}

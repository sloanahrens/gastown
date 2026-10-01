package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// A rig's database name is a gastown fact the rig registry absorbs from the
// rig's .beads/metadata.json (gt-y3pgh.7, D5 Q3). bd reads metadata.json
// itself, so the file stays; gastown reads the registry first and falls
// back to metadata.json only for a rig the registry has no name for (a town
// not yet migrated, or the town's own .beads).

// RigMetadataDatabase is the dolt_database a rig's bd metadata.json names:
// <rig>/mayor/rig/.beads first, then <rig>/.beads; "" when neither names
// one.
func RigMetadataDatabase(townRoot, rig string) string {
	db, _ := rigMetadataDatabase(townRoot, rig)
	return db
}

// rigMetadataDatabase is RigMetadataDatabase and the metadata.json it read,
// relative to the town root.
func rigMetadataDatabase(townRoot, rig string) (db, rel string) {
	for _, dir := range []string{rig + "/mayor/rig/.beads", rig + "/.beads"} {
		if db := metadataDatabase(filepath.Join(townRoot, filepath.FromSlash(dir))); db != "" {
			return db, dir + "/metadata.json"
		}
	}
	return "", ""
}

func metadataDatabase(beadsDir string) string {
	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json")) //nolint:gosec // G304: bd metadata under the town root
	if err != nil {
		return ""
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	return meta.DoltDatabase
}

// RigDatabaseForBeadsDir is the registry's database name for the rig whose
// beads directory is beadsDir (<town>/<rig>/mayor/rig/.beads or
// <town>/<rig>/.beads, redirects already resolved), or "" when beadsDir is
// not a registered rig's or the registry names no database for it.
func RigDatabaseForBeadsDir(beadsDir string) string {
	if filepath.Base(beadsDir) != ".beads" {
		return ""
	}
	rigDir := filepath.Dir(beadsDir)
	if filepath.Base(rigDir) == "rig" && filepath.Base(filepath.Dir(rigDir)) == "mayor" {
		rigDir = filepath.Dir(filepath.Dir(rigDir))
	}
	townRoot, rig := filepath.Dir(rigDir), filepath.Base(rigDir)
	if _, err := os.Stat(filepath.Join(townRoot, "mayor", "town.json")); err != nil {
		return ""
	}
	rc, err := LoadRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"))
	if err != nil {
		return ""
	}
	return rc.Rigs[rig].DoltDatabase
}

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// A rig's bd workspace records its database in .beads/metadata.json, which bd
// reads itself and writes (bd init). The rig registry carries the same name
// for gastown (RigEntry.DoltDatabase); townconfig resolves which one a reader
// gets. These are the only production reads of the field.

// BeadsMetadataFile is bd's per-workspace config file inside a .beads
// directory.
const BeadsMetadataFile = "metadata.json"

// BeadsMetadataDatabase is the dolt_database named by beadsDir's bd
// metadata.json, trimmed, or "" when the file is absent, does not parse, or
// names none. It does not follow a redirect.
func BeadsMetadataDatabase(beadsDir string) string {
	db, _ := BeadsFileDatabase(filepath.Join(beadsDir, BeadsMetadataFile))
	return db
}

// BeadsFileDatabase is the trimmed dolt_database of a bd config file
// (metadata.json, or the legacy config.json bd migrates from). err is nil when
// the file was read and parsed, and otherwise says why it could not be: a
// missing file wraps fs.ErrNotExist, and a read or parse failure names the
// path. A file that does not parse is an error, not a file that names no
// database, so a caller must check err before it reads the name as absent.
func BeadsFileDatabase(path string) (db string, err error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: bd config file of a beads directory gastown chose
	if err != nil {
		return "", err
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	return strings.TrimSpace(meta.DoltDatabase), nil
}

// RigBeadsDirs are the directories, relative to the town root, where a rig's
// bd workspace keeps metadata.json, in the order gastown looks:
// <rig>/mayor/rig/.beads (the tracked workspace), then <rig>/.beads.
func RigBeadsDirs(rig string) []string {
	return []string{constants.RigBeadsPath(rig), rig + "/.beads"}
}

// RigMetadataDatabase is the dolt_database of the first of rig's
// RigBeadsDirs whose metadata.json names one, and that metadata.json's path
// relative to townRoot; ("", "") when none does.
func RigMetadataDatabase(townRoot, rig string) (db, file string) {
	for _, dir := range RigBeadsDirs(rig) {
		if db := BeadsMetadataDatabase(filepath.Join(townRoot, filepath.FromSlash(dir))); db != "" {
			return db, dir + "/" + BeadsMetadataFile
		}
	}
	return "", ""
}

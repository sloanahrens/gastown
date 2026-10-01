package townconfig

// Rig databases (gt-y3pgh.11, D5 Q3).
//
// A rig's database name on the town Dolt server is a gastown fact, and the
// rig registry holds it: the entry's "dolt_database". It came from the
// dolt_database of the rig's bd .beads/metadata.json, which stays, because
// bd reads that file itself. gt rig add records both; gt config migrate and
// gt doctor --fix (rig-database) copy metadata.json's name into an entry
// that has none (AbsorbRigDatabases).
//
// Gastown reads the name only through this file: DatabaseForBeadsDir for a
// workspace, RigDatabaseName or Town.RigDatabase for a rig. The registry
// answers for a registered rig that has the field; metadata.json answers for
// everything else (a town not yet migrated, the town's own .beads, a
// directory that is not a rig's). A registry that does not load also falls
// back to metadata.json: that is the name bd itself will use, and the
// startup gate reports the broken file.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
)

// RigDatabase is a registered rig's database name from its registry entry,
// "" when the entry records none. An unregistered rig is ErrUnknownRig.
func (t *Town) RigDatabase(name string) (string, error) {
	entry, ok := t.rigs[name]
	if !ok {
		return "", fmt.Errorf("%w %q: not in %s", ErrUnknownRig, name, t.path(FileRigs))
	}
	return entry.DoltDatabase, nil
}

// DatabaseForBeadsDir is the database the bd workspace at beadsDir uses, as
// gastown addresses it. For a registered rig's workspace,
// <town>/<rig>/mayor/rig/.beads or <town>/<rig>/.beads, it is the registry's
// dolt_database; otherwise, or when the registry records none, it is the
// dolt_database of beadsDir's metadata.json. "" when neither names one.
// beadsDir is taken as given: callers resolve redirects first.
func DatabaseForBeadsDir(beadsDir string) string {
	if root, rig, ok := rigOfBeadsDir(beadsDir); ok {
		if db := RegistryDatabase(root, rig); db != "" {
			return db
		}
	}
	return config.BeadsMetadataDatabase(beadsDir)
}

// RigDatabaseName is rig's database under the town at root: the registry's
// dolt_database, else the one the rig's bd metadata.json names
// (config.RigMetadataDatabase), else "".
func RigDatabaseName(root, rig string) string {
	if db := RegistryDatabase(root, rig); db != "" {
		return db
	}
	db, _ := config.RigMetadataDatabase(root, rig)
	return db
}

// RegistryDatabase is rig's dolt_database in the registry of the town at
// root alone, "" when the registry does not load, has no such rig or records
// no name. Writers of metadata.json use it to keep bd's copy equal to the
// registry's; readers want RigDatabaseName or DatabaseForBeadsDir.
func RegistryDatabase(root, rig string) string {
	rigs, err := loadRegistry(root)
	if err != nil {
		return ""
	}
	return rigs[rig].DoltDatabase
}

// rigOfBeadsDir names the town root and rig whose workspace beadsDir is,
// by its shape alone; ok is false for any other directory.
func rigOfBeadsDir(beadsDir string) (root, rig string, ok bool) {
	if beadsDir == "" {
		return "", "", false
	}
	beadsDir = filepath.Clean(beadsDir)
	if filepath.Base(beadsDir) != ".beads" {
		return "", "", false
	}
	rigDir := filepath.Dir(beadsDir)
	if filepath.Base(rigDir) == "rig" && filepath.Base(filepath.Dir(rigDir)) == "mayor" {
		rigDir = filepath.Dir(filepath.Dir(rigDir))
	}
	root, rig = filepath.Dir(rigDir), filepath.Base(rigDir)
	if rig == "" || rig == "." || rig == string(filepath.Separator) || root == rigDir {
		return "", "", false
	}
	return root, rig, true
}

// loadRegistry reads the rig registry of the town at root. A root without
// mayor/town.json is ErrNotATown, so no directory is mistaken for a town.
func loadRegistry(root string) (map[string]config.RigEntry, error) {
	if _, err := os.Stat(filepath.Join(root, FileTown)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotATown, err)
	}
	rc, err := config.LoadRigsConfig(filepath.Join(root, FileRigs))
	if err != nil {
		return nil, err
	}
	return rc.Rigs, nil
}

// RigDatabaseState is one registered rig's database name in the registry
// and in the bd metadata.json that bd reads.
type RigDatabaseState struct {
	Rig string
	// Registry is the registry entry's dolt_database.
	Registry string
	// Metadata is the dolt_database of the rig's bd metadata.json, and
	// MetadataFile that file relative to the town root ("" when none
	// names one).
	Metadata, MetadataFile string
}

// Drifted reports whether the registry and metadata.json name different
// databases: gastown and bd would address different databases.
func (s RigDatabaseState) Drifted() bool {
	return s.Registry != "" && s.Metadata != "" && s.Registry != s.Metadata
}

// Absorbable reports whether metadata.json names a database the registry
// entry lacks.
func (s RigDatabaseState) Absorbable() bool {
	return s.Registry == "" && s.Metadata != ""
}

// RigDatabaseStates lists every registered rig's database names, sorted by
// rig. A town without a registry has none.
func RigDatabaseStates(root string) ([]RigDatabaseState, error) {
	rigs, err := loadRegistry(root)
	if errors.Is(err, config.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]RigDatabaseState, 0, len(rigs))
	for _, name := range sortedKeys(rigs) {
		out = append(out, rigDatabaseState(root, name, rigs[name]))
	}
	return out, nil
}

func rigDatabaseState(root, name string, entry config.RigEntry) RigDatabaseState {
	db, file := config.RigMetadataDatabase(root, name)
	return RigDatabaseState{Rig: name, Registry: entry.DoltDatabase, Metadata: db, MetadataFile: file}
}

// AbsorbRigDatabases records in the registry, for every registered rig
// whose entry has no dolt_database, the one its bd metadata.json names, and
// returns those rigs' states as they were before. An entry that already has
// a name keeps it, even when metadata.json names another (gt doctor's
// rig-database check reports that). With dryRun it writes nothing. Running
// it again finds nothing to do. gt config migrate and gt doctor --fix call
// it; it works on either config layout.
func AbsorbRigDatabases(root string, dryRun bool) ([]RigDatabaseState, error) {
	if dryRun {
		states, err := RigDatabaseStates(root)
		if err != nil {
			return nil, err
		}
		var out []RigDatabaseState
		for _, s := range states {
			if s.Absorbable() {
				out = append(out, s)
			}
		}
		return out, nil
	}
	if _, err := os.Stat(filepath.Join(root, FileTown)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotATown, err)
	}
	path := filepath.Join(root, FileRigs)
	var out []RigDatabaseState
	err := config.UpdateConfigJSON(path, 0o644, func(rc *config.RigsConfig, exists bool) error {
		out = nil
		if !exists {
			return errNothingToAbsorb
		}
		for _, name := range sortedKeys(rc.Rigs) {
			entry := rc.Rigs[name]
			s := rigDatabaseState(root, name, entry)
			if !s.Absorbable() {
				continue
			}
			entry.DoltDatabase = s.Metadata
			rc.Rigs[name] = entry
			out = append(out, s)
		}
		if len(out) == 0 {
			return errNothingToAbsorb
		}
		return nil
	})
	if errors.Is(err, errNothingToAbsorb) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// errNothingToAbsorb stops AbsorbRigDatabases' update before it writes.
var errNothingToAbsorb = errors.New("no rig database to absorb")

package config

import "errors"

// errNothingToDelete withholds the write when a delete has nothing to do:
// returning it from the mutate function leaves the file untouched.
var errNothingToDelete = errors.New("nothing to delete")

// SetRigEntry adds or replaces one rig's entry in the registry at path. The
// entry is applied to the file's own contents under its lock, so a rig
// another gt process registered, removed or parked in the meantime is kept
// (gt-4iobv).
func SetRigEntry(path, rigName string, entry RigEntry) error {
	return UpdateConfigJSON(path, 0o600, func(rc *RigsConfig, exists bool) error {
		if !exists {
			rc.Version = CurrentRigsVersion
		}
		if rc.Rigs == nil {
			rc.Rigs = make(map[string]RigEntry)
		}
		rc.Rigs[rigName] = entry
		return nil
	})
}

// DeleteRigEntry removes one rig's entry from the registry at path, keeping
// the other entries as they are on disk. A rig that is not registered there
// is not an error and writes nothing (gt-4iobv).
func DeleteRigEntry(path, rigName string) error {
	err := UpdateConfigJSON(path, 0o600, func(rc *RigsConfig, exists bool) error {
		if !exists {
			return errNothingToDelete
		}
		if _, ok := rc.Rigs[rigName]; !ok {
			return errNothingToDelete
		}
		delete(rc.Rigs, rigName)
		return nil
	})
	if errors.Is(err, errNothingToDelete) {
		return nil
	}
	return err
}

package gocacheprog

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
)

// evictSuffix ends a chosen file's name while the eviction decides its fate.
const evictSuffix = ".evicting"

// evicted is a chosen file renamed aside; gone is its name until it is kept or deleted.
type evicted struct {
	id         []byte
	path, gone string
}

// applyEviction deletes the chosen index files (actions) and bodies (outputs)
// in dir while other processes read and write it. The order rename, reread,
// restore, delete is the safety rule: a chosen file leaves its name before
// reread runs, so an entry that gained a journal record is named by reread and
// kept, and a newer file under the freed name is never overwritten. A file
// already gone is neither an error nor reported. A failed step returns its
// error with what was deleted so far and starts no further delete; a file left
// at its .evicting name is only a cache miss.
func applyEviction(dir string, actions, outputs [][]byte, reread func() (gainedActions, gainedOutputs [][]byte, err error)) (deletedActions, deletedOutputs [][]byte, err error) {
	movedActions, err := renameAside(dir, actions, "-a")
	if err != nil {
		return nil, nil, err
	}
	movedOutputs, err := renameAside(dir, outputs, "-d")
	if err != nil {
		return nil, nil, err
	}
	gainedActions, gainedOutputs, err := reread()
	if err != nil {
		return nil, nil, fmt.Errorf("gocacheprog: evict: reread: %w", err)
	}
	restActions, err := restoreGained(movedActions, gainedActions)
	if err != nil {
		return nil, nil, err
	}
	restOutputs, err := restoreGained(movedOutputs, gainedOutputs)
	if err != nil {
		return nil, nil, err
	}
	deletedActions, err = deleteAside(restActions)
	if err != nil {
		return deletedActions, nil, err
	}
	deletedOutputs, err = deleteAside(restOutputs)
	return deletedActions, deletedOutputs, err
}

// renameAside moves each chosen file to its .evicting name, at the path Put
// wrote it to. A file already gone is skipped.
func renameAside(dir string, ids [][]byte, suffix string) ([]evicted, error) {
	var moved []evicted
	for _, id := range ids {
		if !validID(id) {
			return nil, fmt.Errorf("gocacheprog: evict: id %q is not a non-empty hex string", id)
		}
		path := (&Disk{dir: dir}).file(id, suffix)
		switch err := os.Rename(path, path+evictSuffix); {
		case err == nil:
			moved = append(moved, evicted{id, path, path + evictSuffix})
		case !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("gocacheprog: evict: rename %s: %w", path, err)
		}
	}
	return moved, nil
}

// restoreGained links each gained file back to its name, then removes the
// evicted copy. link refuses to replace a newer file. It returns the files that
// were not gained.
func restoreGained(moved []evicted, gained [][]byte) ([]evicted, error) {
	var rest []evicted
	for _, f := range moved {
		if !slices.ContainsFunc(gained, func(x []byte) bool { return bytes.Equal(x, f.id) }) {
			rest = append(rest, f)
			continue
		}
		switch err := os.Link(f.gone, f.path); {
		case err == nil, errors.Is(err, os.ErrExist):
		case errors.Is(err, os.ErrNotExist):
			continue
		default:
			return nil, fmt.Errorf("gocacheprog: evict: restore %s: %w", f.path, err)
		}
		if err := os.Remove(f.gone); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("gocacheprog: evict: remove %s: %w", f.gone, err)
		}
	}
	return rest, nil
}

// deleteAside removes each evicted file and returns the ids it deleted.
func deleteAside(files []evicted) ([][]byte, error) {
	var deleted [][]byte
	for _, f := range files {
		switch err := os.Remove(f.gone); {
		case err == nil:
			deleted = append(deleted, f.id)
		case !errors.Is(err, os.ErrNotExist):
			return deleted, fmt.Errorf("gocacheprog: evict: delete %s: %w", f.gone, err)
		}
	}
	return deleted, nil
}

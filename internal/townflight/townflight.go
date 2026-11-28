package townflight

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func inflightDir(townRoot string) string { return filepath.Join(townRoot, "daemon", "inflight") }

// Register lists label as a command in flight in townRoot/daemon/inflight and
// returns the function that removes it. The entry is renamed to its .cmd name
// only while locked and labeled; a failed rename drops it.
func Register(townRoot, label string) (func() error, error) {
	return register(os.Rename, townRoot, label)
}

// register is Register with the rename a test can fail.
func register(rename func(oldpath, newpath string) error, townRoot, label string) (func() error, error) {
	e, err := createEntry(inflightDir(townRoot), label)
	if err != nil {
		return nil, err
	}
	path := e.path + ".cmd" // the .cmd suffix is what lists the entry
	if err := rename(e.path, path); err != nil {
		return nil, errors.Join(fmt.Errorf("listing inflight entry: %w", err), e.drop())
	}
	e.path = path
	return e.drop, nil
}

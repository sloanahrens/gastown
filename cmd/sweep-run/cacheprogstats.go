package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// copyCreator makes the file a copy is written to. It must fail when name already exists.
type copyCreator func(name string) (io.WriteCloser, error)

// createCopy makes the copy file exclusively: O_EXCL fails when name exists, so an earlier copy is never overwritten.
func createCopy(name string) (io.WriteCloser, error) {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G302: 0o644 is the mode the run's other logs use
	if err != nil {
		return nil, err
	}
	return f, nil
}

// copyCacheProgStats writes src's bytes to a new file dst, which create makes. A failed read, create, write or
// close is an error naming its path. A write or close failure also drops the copy this call created.
func copyCacheProgStats(src, dst string, create copyCreator) error {
	data, err := os.ReadFile(src) //nolint:gosec // G304: src is the cache program's stats file under the claim this run mounts
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	out, err := create(dst)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	_, writeErr := out.Write(data)
	closeErr := out.Close()
	if writeErr != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", dst, writeErr), closeErr, dropCacheProgCopy(dst))
	}
	if closeErr != nil {
		return errors.Join(fmt.Errorf("closing %s: %w", dst, closeErr), dropCacheProgCopy(dst))
	}
	return nil
}

// dropCacheProgCopy removes a copy this call made; a copy already gone is not an error.
func dropCacheProgCopy(dst string) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) { //nolint:gosec // G703: dst is the copy path this call created
		return fmt.Errorf("dropping the copy %s: %w", dst, err)
	}
	return nil
}

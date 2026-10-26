package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// BackupNameAttempts bounds the -N search for a free backup name. A directory
// holding this many copies of one file is a mistake to report, not to extend.
const BackupNameAttempts = 100

// BackupFile copies path to a sibling named <path>.bak-<stamp> before a write
// replaces it, and returns the copy's path. A path that does not exist yet has
// nothing to keep.
//
// A name already taken is not overwritten: the copy is <path>.bak-<stamp>-2,
// -3, ... instead, because the only copy of a file must not be lost to a second
// write inside one stamp's resolution. More than BackupNameAttempts taken names
// is an error, so a directory that collects copies is reported rather than
// searched forever (gt-jaagc).
func BackupFile(path, stamp string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: internal config path
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("backing up %s: %w", path, err)
	}
	mode := os.FileMode(0o600)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}
	base := filepath.Clean(path) + ".bak-" + stamp
	dst := base
	for n := 2; n <= BackupNameAttempts+1; n++ {
		out, createErr := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(createErr, os.ErrExist) {
			dst = fmt.Sprintf("%s-%d", base, n)
			continue
		}
		if createErr != nil {
			return "", fmt.Errorf("backing up %s: %w", path, createErr)
		}
		_, writeErr := out.Write(data)
		closeErr := out.Close()
		if writeErr != nil {
			return "", fmt.Errorf("backing up %s: %w", path, writeErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("backing up %s: %w", path, closeErr)
		}
		return dst, nil
	}
	return "", fmt.Errorf("backing up %s: %d copies named %s.bak-%s already exist", path, BackupNameAttempts, filepath.Clean(path), stamp)
}

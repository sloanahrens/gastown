// Package atomicfile provides atomic file-write primitives (write-to-temp +
// rename). Kept as a leaf package with no internal dependencies so any other
// package — including low-level ones that util/ transitively depends on — can
// use it without creating an import cycle.
package atomicfile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// osSyncFile flushes the temp file's contents and metadata to the disk.
func osSyncFile(f *os.File) error { return f.Sync() }

// osSyncDir flushes the directory entry the rename added.
func osSyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// dirSyncUnsupported reports whether err is a filesystem declining to sync a
// directory, which is not a durability failure: the rename itself is atomic,
// so the write stands. EINVAL is the answer on macOS and the BSDs; ENOTSUP is
// for the filesystems that say so explicitly.
func dirSyncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}

// WriteJSON writes JSON data to a file atomically with mode 0644.
// It first writes to a temporary file in the same directory, then renames it
// to the target path. This prevents data corruption if the process crashes
// during write. The rename operation is atomic on POSIX systems.
func WriteJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, data, 0644)
}

// WriteJSONWithPerm is like WriteJSON but uses the given file mode.
func WriteJSONWithPerm(path string, v interface{}, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, data, perm)
}

// EnsureDirAndWriteJSON creates parent directories (mode 0755) if needed, then
// atomically writes JSON with mode 0644.
func EnsureDirAndWriteJSON(path string, v interface{}) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return WriteJSON(path, v)
}

// EnsureDirAndWriteJSONWithPerm is like EnsureDirAndWriteJSON but uses the
// given file mode for the output file.
func EnsureDirAndWriteJSONWithPerm(path string, v interface{}, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return WriteJSONWithPerm(path, v, perm)
}

// WriteFile writes data to a file atomically by writing to a unique temp file
// in the same directory and then renaming it over the target. The rename is
// atomic on POSIX systems; concurrent writers each produce self-consistent
// content because each uses a distinct temp file. The temp file is synced
// before the rename and the directory after it, so a power loss leaves the
// destination holding either its old contents or the new ones, never a
// zero-length or truncated file (gt-9rrnq).
func WriteFile(path string, data []byte, perm os.FileMode) error {
	return writeFile(path, data, perm, osSyncFile, osSyncDir)
}

// writeFile is WriteFile with its two syncs injected, so a test can script the
// failure and the refusal the real filesystem will not produce on demand
// (gt-9rrnq).
func writeFile(path string, data []byte, perm os.FileMode, syncFile func(*os.File) error, syncDir func(string) error) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	// "*" in the pattern is replaced with a random suffix by os.CreateTemp,
	// preventing concurrent writers from colliding on the same temp file.
	f, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := f.Name()

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmpName)
		return err
	}

	// CreateTemp uses 0600 by default; apply the caller's permissions.
	if err := os.Chmod(tmpName, perm); err != nil {
		f.Close()
		os.Remove(tmpName)
		return err
	}

	// The bytes must reach the disk before the rename publishes them: a
	// rename that lands first leaves the destination naming an empty file.
	// The failure path removes the temp file, so the destination keeps its
	// previous contents and a retry is safe.
	if err := syncFile(f); err != nil {
		f.Close()
		os.Remove(tmpName)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}

	// Sync the directory so the rename survives a power loss. A filesystem
	// that cannot sync a directory is not a failure (dirSyncUnsupported).
	if err := syncDir(dir); err != nil && !dirSyncUnsupported(err) {
		return err
	}

	return nil
}

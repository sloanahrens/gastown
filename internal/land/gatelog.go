package land

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// GateLogRetention is how long a rejected gate's kept job log survives: a
	// file older than this is deleted when the next log is written.
	GateLogRetention = 14 * 24 * time.Hour
	// gateLogExt is the suffix of a kept log. The prune touches nothing else.
	gateLogExt = ".log"
)

// GateLogDir is the directory a rig's kept gate logs live in:
// <townRoot>/.runtime/gate-logs/<rig>, empty -- so no log is kept -- when
// townRoot is unset or rig is not one plain path element.
func GateLogDir(townRoot, rig string) string {
	if townRoot == "" || !pathElement(rig) {
		return ""
	}
	return filepath.Join(townRoot, ".runtime", "gate-logs", rig)
}

// pathElement reports whether name can be joined into a path as a single
// element: a rig name reaching here is read from town config, but a name
// carrying a separator or a ".." would put kept logs outside the town's
// runtime directory, so it keeps none.
func pathElement(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsRune(name, filepath.Separator) && filepath.VolumeName(name) == ""
}

// gateLogFS is the filesystem a kept log is written and pruned through; a nil
// field is the os function of the same shape. The failure paths are why it
// exists: a directory that refuses a write cannot be built on every host (root
// ignores the permission bits), so a test drives those paths here.
type gateLogFS struct {
	mkdirAll  func(path string, perm os.FileMode) error
	writeFile func(path string, data []byte, perm os.FileMode) error
	readDir   func(dir string) ([]os.DirEntry, error)
	remove    func(path string) error
}

func (fs gateLogFS) mkdir(path string, perm os.FileMode) error {
	if fs.mkdirAll != nil {
		return fs.mkdirAll(path, perm)
	}
	return os.MkdirAll(path, perm)
}

func (fs gateLogFS) write(path string, data []byte, perm os.FileMode) error {
	if fs.writeFile != nil {
		return fs.writeFile(path, data, perm)
	}
	return os.WriteFile(path, data, perm)
}

func (fs gateLogFS) list(dir string) ([]os.DirEntry, error) {
	if fs.readDir != nil {
		return fs.readDir(dir)
	}
	return os.ReadDir(dir)
}

func (fs gateLogFS) rm(path string) error {
	if fs.remove != nil {
		return fs.remove(path)
	}
	return os.Remove(path)
}

// keepGateLog writes log to dir as <short-sha>-<unix-time>.log, deletes the
// logs whose mtime is more than GateLogRetention before now, and returns the
// path written. now is the wall clock the file name and the retention share:
// a test with a fixed clock dates the files it stages in that same clock.
//
// A write that fails returns no path: the caller's rejection stands and has no
// log to name. A prune that fails after the write returns the path with the
// error, because the new log is kept and worth naming either way.
func keepGateLog(fs gateLogFS, dir, sha, log string, now time.Time) (string, error) {
	if err := fs.mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating the gate log directory %s: %w", dir, err)
	}
	// The unix time in nanoseconds: it is the unix time the bead names, at a
	// resolution where two rejections in one second cannot overwrite each
	// other's log (gt-wwqx8).
	name := fmt.Sprintf("%s-%d%s", shortSHA(sha), now.UnixNano(), gateLogExt)
	path := filepath.Join(dir, name)
	// The log is private: CI output can carry what a failing step printed,
	// environment lines included.
	if err := fs.write(path, []byte(log), 0o600); err != nil {
		return "", fmt.Errorf("writing the gate job log %s: %w", path, err)
	}
	if err := pruneGateLogs(fs, dir, now); err != nil {
		return path, fmt.Errorf("pruning the gate job logs older than %s: %w", GateLogRetention, err)
	}
	return path, nil
}

// pruneGateLogs deletes the kept logs in dir that are older than
// GateLogRetention. A file it cannot read or remove is reported and the rest
// are still tried, so one stuck file never blocks the cleanup of the others.
func pruneGateLogs(fs gateLogFS, dir string, now time.Time) error {
	entries, err := fs.list(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), gateLogExt) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if now.Sub(info.ModTime()) <= GateLogRetention {
			continue
		}
		if err := fs.rm(filepath.Join(dir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

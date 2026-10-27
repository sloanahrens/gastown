package land

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// GateLogRetention is how long a rejected gate's kept job log survives: a
	// log whose name dates it more than this before the new one's clock is
	// deleted when the next log is written.
	GateLogRetention = 14 * 24 * time.Hour
	// gateLogExt is the suffix of a kept log. The prune touches nothing else.
	gateLogExt = ".log"
	// gateLogNanoFloor is the smallest stamp a kept log's name can carry in
	// nanoseconds. A unix time in seconds stays below it for tens of thousands
	// of years, a unix time in nanoseconds passed it in 1970, so one comparison
	// tells the two resolutions apart (gt-zua7o).
	gateLogNanoFloor = int64(1_000_000_000_000)
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
// logs whose name dates them more than GateLogRetention before now, and returns
// the path written. now is the one clock the file name and the retention share,
// so the prune reads the time the name carries and never a filesystem mtime
// (gt-zua7o).
//
// Two rejections of the same commit in the same second name one file: the
// later log replaces the earlier, which is that same commit's gate job log. A
// different commit carries a different short sha.
//
// A write that fails returns no path: the caller's rejection stands and has no
// log to name. A prune that fails after the write returns the path with the
// error, because the new log is kept and worth naming either way.
func keepGateLog(fs gateLogFS, dir, sha, log string, now time.Time) (string, error) {
	if err := fs.mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating the gate log directory %s: %w", dir, err)
	}
	name := fmt.Sprintf("%s-%d%s", shortSHA(sha), now.Unix(), gateLogExt)
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

// pruneGateLogs deletes the kept logs in dir that are more than
// GateLogRetention before now, by the time their name carries: the clock that
// named the log is the clock that dates it, never a filesystem mtime from
// another one (gt-zua7o). A name that dates nothing is a file the gate did not
// write and is left alone. A file it cannot remove is reported and the rest are
// still tried, so one stuck file never blocks the cleanup of the others.
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
		at, ok := gateLogTime(entry.Name())
		if !ok || now.Sub(at) <= GateLogRetention {
			continue
		}
		if err := fs.rm(filepath.Join(dir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// gateLogTime is when a kept log's name says it was written: the unix time
// keepGateLog puts after the short sha, in nanoseconds for the logs kept before
// gt-zua7o. It reports false for a name that carries none.
func gateLogTime(name string) (time.Time, bool) {
	stamp := strings.TrimSuffix(name, gateLogExt)
	dash := strings.LastIndexByte(stamp, '-')
	if dash < 0 {
		return time.Time{}, false
	}
	v, err := strconv.ParseInt(stamp[dash+1:], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	// A legacy name read as seconds would date its log to the year 58 million,
	// so the prune would keep it forever. Reading both resolutions is what
	// retires those files (gt-zua7o).
	if v >= gateLogNanoFloor {
		return time.Unix(0, v), true
	}
	return time.Unix(v, 0), true
}

package tiersweep

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PruneMaxAge is how long a finished sweep's default log directory is kept: a
// RED run's is the only copy of its per-package logs, so a sweep deletes one
// only long after its failing names were read.
const PruneMaxAge = 48 * time.Hour

// PruneEntry is one log directory a prune would delete: its path, its age at
// the clock the prune was given, and its own size in bytes, not its contents'.
type PruneEntry struct {
	Path string
	Age  time.Duration
	Size int64
}

// PruneCandidates lists the entries under dir a sweep may delete: real
// directories, named for the default log directory pattern, older than maxAge,
// sorted by path. A symlink is never one, even when it points at a directory:
// the Lstat answer carries the link, not the target (gt-xk9my). The run's own
// log directory is not among them either, since the run made it a moment ago
// and maxAge a sweep passes is far longer than that.
func PruneCandidates(host Host, dir string, now time.Time, maxAge time.Duration) ([]PruneEntry, error) {
	entries, err := host.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []PruneEntry
	for _, entry := range entries {
		name := entry.Name()
		if !isLogDirName(name) {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := host.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			continue
		}
		age := now.Sub(info.ModTime())
		if age <= maxAge {
			continue
		}
		out = append(out, PruneEntry{Path: path, Age: age, Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Prune deletes the directories PruneCandidates lists and returns the paths it
// removed, in the same sorted order. Every candidate is tried even when one
// cannot be deleted: the first error is returned beside the removals that
// succeeded, so one stuck directory does not keep the rest.
func Prune(host Host, dir string, now time.Time, maxAge time.Duration) (removed []string, err error) {
	candidates, err := PruneCandidates(host, dir, now, maxAge)
	if err != nil {
		return nil, err
	}
	var firstErr error
	for _, c := range candidates {
		if err := host.RemoveAll(c.Path); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed = append(removed, c.Path)
	}
	return removed, firstErr
}

// isLogDirName reports whether name is a default log directory's: the pattern
// followed by at least one character, and no path separator, so a listing can
// never point the prune outside dir.
func isLogDirName(name string) bool {
	rest, ok := strings.CutPrefix(name, logDirPattern)
	if !ok || rest == "" {
		return false
	}
	return !strings.ContainsAny(rest, `/\`)
}

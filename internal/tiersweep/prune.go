package tiersweep

import (
	"errors"
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
// the Lstat answer carries the link, not the target. An entry that vanished
// between the listing and its Lstat is skipped, since a concurrent sweep may
// have pruned it first (gt-xk9my).
//
// The name test decides every candidate: only the pattern followed by at least
// one character, with no path separator, so a listing can never point the prune
// outside dir. On a shared TMPDIR another user's matching directory is a
// candidate too, but the sticky bit makes its RemoveAll fail, which Prune
// reports like any other stuck directory.
func PruneCandidates(host Host, dir string, now time.Time, maxAge time.Duration) ([]PruneEntry, error) {
	return pruneCandidates(host, dir, now, maxAge, "")
}

// pruneCandidates is PruneCandidates with one path held back: the sweep's own
// log directory, which Run passes as skip. Run keeps it out by name rather than
// by age, so the rule holds however old its mtime is (gt-xk9my). An empty skip
// holds nothing back.
func pruneCandidates(host Host, dir string, now time.Time, maxAge time.Duration, skip string) ([]PruneEntry, error) {
	entries, err := host.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	skip = filepath.Clean(skip)
	var out []PruneEntry
	for _, entry := range entries {
		name := entry.Name()
		if !isLogDirName(name) {
			continue
		}
		path := filepath.Join(dir, name)
		if skip == filepath.Clean(path) {
			continue
		}
		info, err := host.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
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
	return pruneExcept(host, dir, now, maxAge, "")
}

// pruneExcept is Prune with one path held back, as pruneCandidates holds it
// back: the sweep's own log directory is never removed by a prune.
func pruneExcept(host Host, dir string, now time.Time, maxAge time.Duration, skip string) (removed []string, err error) {
	candidates, err := pruneCandidates(host, dir, now, maxAge, skip)
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

// RemoveListed deletes the entries of one earlier listing and nothing else. It
// never lists a directory: the operator approved these paths, so a directory
// that appeared after the listing is out of reach by construction. Each path
// is checked again with an Lstat before it goes, by the rule PruneCandidates
// applies, because the listing may be minutes old: a path that vanished, was
// replaced by a symlink or a file, was touched since (younger than maxAge) or
// whose name is not a log directory's is left alone and returned in stale.
// An Lstat that fails for another reason is an error and the path stays. Every
// path is tried; the first error is returned beside what was removed.
func RemoveListed(host Host, listed []PruneEntry, now time.Time, maxAge time.Duration) (removed, stale []string, err error) {
	var firstErr error
	note := func(e error) {
		if firstErr == nil {
			firstErr = e
		}
	}
	for _, entry := range listed {
		path := filepath.Clean(entry.Path)
		if !isLogDirName(filepath.Base(path)) {
			stale = append(stale, entry.Path)
			continue
		}
		info, lerr := host.Lstat(path)
		if errors.Is(lerr, fs.ErrNotExist) {
			stale = append(stale, entry.Path)
			continue
		}
		if lerr != nil {
			note(lerr)
			continue
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() || now.Sub(info.ModTime()) <= maxAge {
			stale = append(stale, entry.Path)
			continue
		}
		if rerr := host.RemoveAll(path); rerr != nil {
			note(rerr)
			continue
		}
		removed = append(removed, entry.Path)
	}
	return removed, stale, firstErr
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

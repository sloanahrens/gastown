package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// sandboxTranscriptArchiveCap is the most the transcript archive keeps, in bytes.
const sandboxTranscriptArchiveCap int64 = 512 << 20

// pruneTranscriptArchive prunes the archive under root, laid out as
// <rig>/<polecat>/<file>: it removes the regular files older than keep, then the
// oldest of those left until they fit under capBytes, and removes a seat
// directory left empty. A missing root holds none; an entry that is not a
// directory where a rig or seat belongs is an error naming it, not skipped.
func pruneTranscriptArchive(root string, now time.Time, keep time.Duration, capBytes int64) error {
	return pruneTranscriptArchiveWith(osArchiveFS, root, now, keep, capBytes)
}

// archiveFS is the filesystem the prune reads and removes through, so a test
// can fail one call, which no real directory state does as root on every host.
type archiveFS struct {
	readDir func(name string) ([]os.DirEntry, error)
	remove  func(name string) error
	info    func(e fs.DirEntry) (fs.FileInfo, error)
}

// osArchiveFS is the real filesystem.
var osArchiveFS = archiveFS{os.ReadDir, os.Remove, fs.DirEntry.Info}

// pruneTranscriptArchiveWith is pruneTranscriptArchive over fsys. A file whose
// removal fails still exists, so it stays in the total the cap is held to.
func pruneTranscriptArchiveWith(fsys archiveFS, root string, now time.Time, keep time.Duration, capBytes int64) error {
	rigs, err := fsys.readDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", root, err)
	}
	type transcript struct {
		path string
		mod  time.Time
		size int64
	}
	var kept []transcript
	var seats []string
	var errs []error
	for _, rig := range rigs {
		rigDir := filepath.Join(root, rig.Name())
		if !rig.IsDir() {
			errs = append(errs, fmt.Errorf("%s is not a directory", rigDir))
			continue
		}
		seatEntries, err := fsys.readDir(rigDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading %s: %w", rigDir, err))
			continue
		}
		for _, seat := range seatEntries {
			seatDir := filepath.Join(rigDir, seat.Name())
			if !seat.IsDir() {
				errs = append(errs, fmt.Errorf("%s is not a directory", seatDir))
				continue
			}
			entries, err := fsys.readDir(seatDir)
			if err != nil {
				errs = append(errs, fmt.Errorf("reading %s: %w", seatDir, err))
				continue
			}
			seats = append(seats, seatDir)
			for _, e := range entries {
				if !e.Type().IsRegular() {
					continue
				}
				p := filepath.Join(seatDir, e.Name())
				info, err := fsys.info(e)
				if err != nil {
					errs = append(errs, fmt.Errorf("reading %s: %w", p, err))
					continue
				}
				if now.Sub(info.ModTime()) > keep {
					if err := fsys.remove(p); err != nil {
						errs = append(errs, fmt.Errorf("removing %s: %w", p, err))
						kept = append(kept, transcript{p, info.ModTime(), info.Size()})
					}
					continue
				}
				kept = append(kept, transcript{p, info.ModTime(), info.Size()})
			}
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].mod.Before(kept[j].mod) })
	var total int64
	for _, f := range kept {
		total += f.size
	}
	for _, f := range kept {
		if total <= capBytes {
			break
		}
		if err := fsys.remove(f.path); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", f.path, err))
			continue
		}
		total -= f.size
	}
	for _, seatDir := range seats {
		entries, err := fsys.readDir(seatDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading %s: %w", seatDir, err))
			continue
		}
		if len(entries) == 0 {
			if err := fsys.remove(seatDir); err != nil {
				errs = append(errs, fmt.Errorf("removing %s: %w", seatDir, err))
			}
		}
	}
	return errors.Join(errs...)
}

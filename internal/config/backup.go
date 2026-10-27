package config

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// BackupNameAttempts bounds the -N search for a free backup name. A directory
// holding this many copies of one file is a mistake to report, not to extend.
const BackupNameAttempts = 100

// The copies kept beside a config file after each backup: the newest
// backupRetentionCount, and every copy younger than backupRetentionAge. A copy
// is written on every config change and the changes are frequent, so a
// directory with nothing pruning it grows without limit (gt-9tx8h). A week
// covers an operator back from a holiday; the count covers a busy afternoon of
// model switches.
const (
	backupRetentionCount = 30
	backupRetentionAge   = 7 * 24 * time.Hour
)

// backupWrite creates dst and writes data to it, and removes its own partial
// file when it fails. The default fsyncs the file before closing it; a test
// injects one that writes short, which the size check must catch.
type backupWrite func(dst string, data []byte, mode os.FileMode) error

// backupRun is what one backup runs with: the clock the retention window reads,
// the copy writer, and the pruner. BackupFile passes the real ones; a test
// passes a fixed clock, a writer that writes short, or a pruner that fails.
// logf reports a prune failure, which never fails the write; BackupFile leaves
// it nil for log.Printf, and a test collects what is reported.
type backupRun struct {
	now   func() time.Time
	write backupWrite
	prune func(path, keep string, now time.Time) error
	logf  func(format string, args ...any)
}

// BackupFile copies path to a sibling named <path>.bak-<stamp> before a write
// replaces it, fsyncs and size-checks the copy, prunes the copies past the
// retention window, and returns the copy's path. A failed or short copy is an
// error, so a caller that keeps its write behind this call leaves the file
// alone. A path that does not exist yet has nothing to keep.
//
// A name already taken is not overwritten: the copy is <path>.bak-<stamp>-2,
// -3, ... instead, because the only copy of a file must not be lost to a second
// write inside one stamp's resolution. More than BackupNameAttempts taken names
// is an error, so a directory that collects copies is reported rather than
// searched forever (gt-jaagc).
func BackupFile(path, stamp string) (string, error) {
	return backupFile(path, stamp, backupRun{now: time.Now, write: writeBackupCopy, prune: pruneCopies})
}

// backupFile is BackupFile with its clock, writer and pruner passed in: the
// retention window needs a fixed clock in a test, a short copy a writer that
// writes part of the data, and a prune failure a pruner that reports one.
func backupFile(path, stamp string, run backupRun) (string, error) {
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
		writeErr := run.write(dst, data, mode)
		if errors.Is(writeErr, os.ErrExist) {
			dst = fmt.Sprintf("%s-%d", base, n)
			continue
		}
		if writeErr != nil {
			return "", fmt.Errorf("backing up %s: %w", path, writeErr)
		}
		if sizeErr := checkCopySize(dst, int64(len(data))); sizeErr != nil {
			removeCopy(dst)
			return "", fmt.Errorf("backing up %s: %w", path, sizeErr)
		}
		if pruneErr := run.prune(path, dst, run.now()); pruneErr != nil {
			report := run.logf
			if report == nil {
				report = log.Printf
			}
			report("backing up %s: pruning old copies: %v", path, pruneErr)
		}
		return dst, nil
	}
	return "", fmt.Errorf("backing up %s: %d copies named %s.bak-%s already exist", path, BackupNameAttempts, filepath.Clean(path), stamp)
}

// writeBackupCopy is the default backupWrite: create the copy fresh, write all
// of data, flush it to disk, and close it. A failure after the create removes
// the partial copy, which is this call's to remove and nobody else's: a file
// already at dst made the create fail with ErrExist instead.
func writeBackupCopy(dst string, data []byte, mode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // G304: internal config path
	if err != nil {
		return err
	}
	return writeAndSync(out, dst, data)
}

// writeAndSync writes data to the copy, flushes it to disk and closes it,
// removing the partial copy when any of that fails.
func writeAndSync(out *os.File, dst string, data []byte) error {
	_, err := out.Write(data)
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		removeCopy(dst)
		return err
	}
	return nil
}

// checkCopySize reports a copy that is not the length of the source, which is
// what a write cut short leaves behind.
func checkCopySize(dst string, want int64) error {
	fi, err := os.Stat(dst)
	if err != nil {
		return err
	}
	if fi.Size() != want {
		return fmt.Errorf("the copy is %d bytes, want %d", fi.Size(), want)
	}
	return nil
}

// removeCopy deletes a copy this call wrote, so a short one is not left for an
// operator to restore.
func removeCopy(dst string) {
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("removing a failed backup %s: %v", dst, err)
	}
}

// pruneCopies is the pruner BackupFile runs: the retention window over the
// directory that holds path.
func pruneCopies(path, keep string, now time.Time) error {
	return pruneBackups(path, keep, now, os.ReadDir)
}

// pruneBackups removes the copies of path in its directory that are past the
// retention window: outside the newest backupRetentionCount and older than
// backupRetentionAge. keep, the copy just written, is never removed, and no
// name that is not <path>.bak-* is touched. It returns the errors it met, which
// the caller logs: a directory that cannot be pruned must not cost the write.
// readDir is os.ReadDir, so a test can hand it a directory it cannot read or an
// entry it cannot stat.
func pruneBackups(path, keep string, now time.Time, readDir func(string) ([]fs.DirEntry, error)) error {
	dir := filepath.Dir(path)
	prefix := filepath.Base(filepath.Clean(path)) + ".bak-"
	entries, err := readDir(dir)
	if err != nil {
		return err
	}
	type oldCopy struct {
		path string
		mod  time.Time
	}
	var copies []oldCopy
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		fi, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed between the read and the stat
		}
		if err != nil {
			return err
		}
		copies = append(copies, oldCopy{filepath.Join(dir, e.Name()), fi.ModTime()})
	}
	slices.SortFunc(copies, func(a, b oldCopy) int {
		if c := b.mod.Compare(a.mod); c != 0 {
			return c
		}
		return cmp.Compare(a.path, b.path)
	})
	var errs []error
	for i, c := range copies {
		if i < backupRetentionCount || now.Sub(c.mod) < backupRetentionAge {
			continue
		}
		if c.path == keep {
			continue
		}
		if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

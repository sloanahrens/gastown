package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// cacheProgDirName is the directory under the cache claim that holds the cache program's stats.
	cacheProgDirName = "gtprog"
	// cacheProgStatsName is the stats file the cache program appends to.
	cacheProgStatsName = "stats.jsonl"
	// cacheProgStatsCopyName is the copy a run leaves in its log directory.
	cacheProgStatsCopyName = "gocacheprog-stats.jsonl"
	// cacheProgStatsMaxBytes is the largest stats file copied: 8 MiB. A larger one is removed uncopied.
	cacheProgStatsMaxBytes int64 = 8 << 20
)

// moveCacheProgStats copies the cache program's stats file from cacheRoot into logDir, then removes the
// source. The copy comes first because the two can sit on different filesystems. A missing source is nil.
// A source over the limit is removed uncopied and the error says so. Every other failure is an error; a
// source that cannot be removed after its copy drops the copy, so a later run does not count its lines
// twice. The caller passes os.Remove as remove.
func moveCacheProgStats(cacheRoot, logDir string, remove func(string) error) error {
	if remove == nil {
		return errors.New("moving the cache program stats: no removal given")
	}
	src := filepath.Join(cacheRoot, cacheProgDirName, cacheProgStatsName)
	dst := filepath.Join(logDir, cacheProgStatsCopyName)
	info, err := os.Stat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; nothing was moved", src)
	}
	if info.Size() > cacheProgStatsMaxBytes {
		if err := remove(src); err != nil {
			return fmt.Errorf("%s is over the %d-byte limit and removing it failed: %w", src, cacheProgStatsMaxBytes, err)
		}
		return fmt.Errorf("%s is %d bytes, over the %d-byte limit: removed uncopied", src, info.Size(), cacheProgStatsMaxBytes)
	}
	if err := copyCacheProgStats(src, dst); err != nil {
		return err
	}
	if err := remove(src); err != nil {
		return errors.Join(fmt.Errorf("removing %s after copying it: %w", src, err), dropCacheProgCopy(dst))
	}
	return nil
}

// copyCacheProgStats writes src's bytes to a new file dst. An existing dst is never overwritten, and a copy
// that fails after dst is created is dropped.
func copyCacheProgStats(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // G304: src is under the cache claim this run mounts
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G302: 0o644 is the mode the run's other logs use
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	_, writeErr := out.Write(data)
	if err := errors.Join(writeErr, out.Close()); err != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", dst, err), dropCacheProgCopy(dst))
	}
	return nil
}

// dropCacheProgCopy removes a copy this run made; a copy already gone is not an error.
func dropCacheProgCopy(dst string) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) { //nolint:gosec // G703: dst is the run's log directory joined with a fixed name
		return fmt.Errorf("dropping the copy %s: %w", dst, err)
	}
	return nil
}

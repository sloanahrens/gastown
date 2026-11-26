package sweeprunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// CacheTrimFileName is the cache trim record's name beside ResultFileName in the
// run's output directory (gt-fp66p0).
const CacheTrimFileName = "cache-trim.json"

// cacheTrimMaxBytes bounds what ReadCacheTrim loads: the record is two numbers,
// and the container writes this directory.
const cacheTrimMaxBytes = 4096

// CacheTrimRecord is a run's cache trim outcome: the cap the sweep cache was
// trimmed to, and the bytes it still held over that cap.
type CacheTrimRecord struct {
	CapBytes  int64 `json:"cap_bytes"`
	OverBytes int64 `json:"over_bytes"`
}

// WriteCacheTrim records r as cache-trim.json under dir in one rename, and
// refuses what ReadCacheTrim would refuse.
func WriteCacheTrim(dir string, r CacheTrimRecord) error {
	if err := validateCacheTrim(r); err != nil {
		return fmt.Errorf("sweeprunner: write cache trim: %w", err)
	}
	path := filepath.Join(dir, CacheTrimFileName)
	if err := atomicfile.WriteJSON(path, r); err != nil {
		return fmt.Errorf("sweeprunner: write cache trim %s: %w", path, err)
	}
	return nil
}

// ReadCacheTrim reads the record in dir. A missing file is no record (zero, false,
// nil); any other unusable file is an error naming it, never a clean absence.
func ReadCacheTrim(dir string) (CacheTrimRecord, bool, error) {
	path := filepath.Join(dir, CacheTrimFileName)
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return CacheTrimRecord{}, false, nil
	case err != nil:
		return CacheTrimRecord{}, false, fmt.Errorf("sweeprunner: read cache trim %s: %w", path, err)
	case !info.Mode().IsRegular():
		return CacheTrimRecord{}, false, fmt.Errorf("sweeprunner: read cache trim %s: not a regular file; refusing to read it", path)
	}
	// The open takes the same guards readResultFile does (gt-u00wo).
	open := func(p string) (io.ReadCloser, error) {
		return os.OpenFile(p, os.O_RDONLY|oNoFollow|oNonBlock, 0) //nolint:gosec // G304: the caller's run directory plus CacheTrimFileName
	}
	r, err := readCacheTrimFile(path, open)
	return r, err == nil, err
}

// readCacheTrimFile decodes and validates the record at path, reading at most
// cacheTrimMaxBytes of it. open is a parameter so a test can make it fail.
func readCacheTrimFile(path string, open func(string) (io.ReadCloser, error)) (CacheTrimRecord, error) {
	rc, err := open(path)
	if err != nil {
		return CacheTrimRecord{}, fmt.Errorf("sweeprunner: read cache trim %s: %w", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(rc, cacheTrimMaxBytes+1))
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return CacheTrimRecord{}, fmt.Errorf("sweeprunner: read cache trim %s: %w", path, err)
	}
	if len(data) > cacheTrimMaxBytes {
		return CacheTrimRecord{}, fmt.Errorf("sweeprunner: read cache trim %s: larger than %d bytes", path, cacheTrimMaxBytes)
	}
	var r CacheTrimRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return CacheTrimRecord{}, fmt.Errorf("sweeprunner: read cache trim %s: not a complete JSON document: %w", path, err)
	}
	if err := validateCacheTrim(r); err != nil {
		return CacheTrimRecord{}, fmt.Errorf("sweeprunner: read cache trim %s: %w", path, err)
	}
	return r, nil
}

// validateCacheTrim refuses a negative figure: neither a cap nor a byte count can be below zero.
func validateCacheTrim(r CacheTrimRecord) error {
	if r.CapBytes < 0 {
		return fmt.Errorf("cap_bytes %d is negative", r.CapBytes)
	}
	if r.OverBytes < 0 {
		return fmt.Errorf("over_bytes %d is negative", r.OverBytes)
	}
	return nil
}

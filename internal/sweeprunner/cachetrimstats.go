package sweeprunner

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// CacheTrimStatsFileName is the trim's one-line stats record beside ResultFileName in the run's log directory.
const CacheTrimStatsFileName = "cache-trim-stats.log"

// cacheTrimStatsMaxBytes bounds what ReadCacheTrimStats loads: the line is a dozen numbers.
const cacheTrimStatsMaxBytes = 4096

// cacheTrimStatsFields is every field of the line, in the order cmd/sweep-run's writeCacheTrimStats writes them.
var cacheTrimStatsFields = []string{"total_bytes", "planned_files", "planned_bytes", "removed_files", "removed_bytes", "absent_files", "failed_removals", "young_bytes", "protected_bytes", "over_bytes", "cap_bytes", "elapsed_ms"}

// ReadCacheTrimStats reads the stats line in logDir under the field names it was
// written with, except total_bytes, returned as claim_bytes_before; a missing file
// is no record (nil, false, nil), and any other unusable file is an error naming it.
func ReadCacheTrimStats(logDir string) (map[string]int64, bool, error) {
	path := filepath.Join(logDir, CacheTrimStatsFileName)
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("sweeprunner: read cache trim stats %s: %w", path, err)
	case !info.Mode().IsRegular():
		return nil, false, fmt.Errorf("sweeprunner: read cache trim stats %s: not a regular file; refusing to read it", path)
	}
	// The open takes the same guards ReadCacheTrim does (gt-u00wo).
	open := func(p string) (io.ReadCloser, error) {
		return os.OpenFile(p, os.O_RDONLY|oNoFollow|oNonBlock, 0) //nolint:gosec // G304: the caller's log directory plus CacheTrimStatsFileName
	}
	stats, err := readCacheTrimStatsFile(path, open)
	return stats, err == nil, err
}

// readCacheTrimStatsFile loads and parses the stats line at path, reading at most
// cacheTrimStatsMaxBytes of it. open is a parameter so a test can make it fail.
func readCacheTrimStatsFile(path string, open func(string) (io.ReadCloser, error)) (map[string]int64, error) {
	rc, err := open(path)
	if err != nil {
		return nil, fmt.Errorf("sweeprunner: read cache trim stats %s: %w", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(rc, cacheTrimStatsMaxBytes+1))
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("sweeprunner: read cache trim stats %s: %w", path, err)
	}
	return parseCacheTrimStats(path, data)
}

// parseCacheTrimStats decodes the one line writeCacheTrimStats writes and refuses
// every other shape, naming path in the error.
func parseCacheTrimStats(path string, data []byte) (map[string]int64, error) {
	errf := func(format string, a ...any) error {
		return fmt.Errorf("sweeprunner: read cache trim stats %s: "+format, append([]any{path}, a...)...)
	}
	switch {
	case len(data) > cacheTrimStatsMaxBytes:
		return nil, errf("larger than %d bytes", cacheTrimStatsMaxBytes)
	case len(data) == 0 || data[len(data)-1] != '\n':
		return nil, errf("no final newline")
	}
	line := string(data[:len(data)-1])
	if strings.Contains(line, "\n") {
		return nil, errf("more than one line")
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "stats" {
		return nil, errf("not a stats line")
	}
	got := make(map[string]int64, len(cacheTrimStatsFields))
	for _, f := range fields[1:] {
		name, value, ok := strings.Cut(f, "=")
		if !ok || !slices.Contains(cacheTrimStatsFields, name) {
			return nil, errf("unknown or malformed field %q", f)
		}
		if _, dup := got[name]; dup {
			return nil, errf("repeated field %q", name)
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return nil, errf("field %q is not a non-negative number: %q", name, value)
		}
		got[name] = n
	}
	for _, name := range cacheTrimStatsFields {
		if _, ok := got[name]; !ok {
			return nil, errf("missing field %q", name)
		}
	}
	got["claim_bytes_before"] = got["total_bytes"]
	delete(got, "total_bytes")
	return got, nil
}

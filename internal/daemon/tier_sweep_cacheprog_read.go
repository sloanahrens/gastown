package daemon

import (
	"errors"
	"fmt"
	"io/fs"
)

// sweepCacheProgStatsMaxBytes bounds a sweep run's gocacheprog-stats.jsonl: one
// JSON line per cache request, and 8 MiB is far past a run's.
const sweepCacheProgStatsMaxBytes = 8 << 20

// readSweepCacheProgStats reads a sweep run's gocacheprog-stats.jsonl at path.
// Only an absent file is a clean answer (present false, no error); a path that
// is not a regular file, a file over sweepCacheProgStatsMaxBytes, a failed stat
// or read, and a read short of the stat'd size are present true with an error
// naming path, and data comes back only when the file was read whole. stat and
// read are parameters, so tests drive every refusal without a real file.
func readSweepCacheProgStats(path string, stat func(string) (fs.FileInfo, error), read func(string) ([]byte, error)) ([]byte, bool, error) {
	info, err := stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, true, fmt.Errorf("read sweep cache program stats %s: stat: %w", path, err)
	case !info.Mode().IsRegular():
		return nil, true, fmt.Errorf("read sweep cache program stats %s: not a regular file", path)
	case info.Size() > sweepCacheProgStatsMaxBytes:
		return nil, true, fmt.Errorf("read sweep cache program stats %s: %d bytes, over the %d byte limit", path, info.Size(), int64(sweepCacheProgStatsMaxBytes))
	}
	data, err := read(path)
	switch {
	case err != nil:
		return nil, true, fmt.Errorf("read sweep cache program stats %s: read: %w", path, err)
	case int64(len(data)) != info.Size():
		return nil, true, fmt.Errorf("read sweep cache program stats %s: read %d bytes, stat said %d; not read whole", path, len(data), info.Size())
	}
	return data, true, nil
}

package gocacheprog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// evictInterval is the least time between two evictions, counted from the last
// write to the index snapshot.
const evictInterval = 10 * time.Minute

// evictGate decides whether this process may evict from dir at now. It takes
// the exclusive evict.lock without blocking, so one process evicts at a time,
// and refuses while index.jsonl was modified less than evictInterval ago. run is
// true only when the lock is held, and then release frees it; otherwise release
// is nil. A lock another holder has, or a fresh snapshot, is a refusal with no
// error. A lock or stat failure is returned as an error, with the lock released.
func evictGate(dir string, now time.Time) (release func(), run bool, err error) {
	release, err = lock.FlockTryAcquireStable(filepath.Join(dir, "evict.lock"))
	switch {
	case errors.Is(err, lock.ErrFlockHeld):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("gocacheprog: evict gate: lock: %w", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "index.jsonl"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return release, true, nil
	case err != nil:
		release()
		return nil, false, fmt.Errorf("gocacheprog: evict gate: stat snapshot: %w", err)
	case now.Sub(fi.ModTime()) < evictInterval:
		release()
		return nil, false, nil
	}
	return release, true, nil
}

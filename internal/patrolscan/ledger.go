package patrolscan

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// FileLedger is the Ledger the daemon uses: one small JSON map of
// "<rig>/<bead>" to the last report time, under the town runtime directory,
// so the once-per-window rule survives daemon restarts. Entries older than
// keep are dropped on write.
type FileLedger struct {
	path string
	keep time.Duration

	mu      sync.Mutex
	loaded  bool
	entries map[string]time.Time
	loadErr error
}

// LedgerPath is the ledger file for a town.
func LedgerPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "patrol_scan", "stranded_reports.json")
}

// NewFileLedger returns a ledger at path that forgets entries older than keep.
func NewFileLedger(path string, keep time.Duration) *FileLedger {
	return &FileLedger{path: path, keep: keep}
}

func (l *FileLedger) load() {
	if l.loaded {
		return
	}
	l.loaded = true
	l.entries = map[string]time.Time{}
	data, err := os.ReadFile(l.path) //nolint:gosec // G304: town runtime path
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			l.loadErr = err
		}
		return
	}
	if err := json.Unmarshal(data, &l.entries); err != nil {
		l.loadErr = err
		l.entries = map[string]time.Time{}
	}
}

// LastReported returns when key was last reported. An unreadable ledger
// reports every key as just reported: a lost ledger must not turn into a
// comment storm, and the next successful write repairs it.
func (l *FileLedger) LastReported(key string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	if l.loadErr != nil {
		return time.Now(), true
	}
	t, ok := l.entries[key]
	return t, ok
}

// MarkReported records a report of key at at and writes the ledger.
func (l *FileLedger) MarkReported(key string, at time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	if l.loadErr != nil {
		return fmt.Errorf("stranded-report ledger %s unreadable, not overwriting: %w", l.path, l.loadErr)
	}
	l.entries[key] = at.UTC()
	if l.keep > 0 {
		for k, t := range l.entries {
			if at.Sub(t) > l.keep {
				delete(l.entries, k)
			}
		}
	}
	return atomicfile.EnsureDirAndWriteJSONWithPerm(l.path, l.entries, 0o644)
}

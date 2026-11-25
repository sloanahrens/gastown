package daemon

// The alert clock remembers when an attention item was last raised: one small
// file per item under a directory the caller names, so a "once per window"
// rule survives the daemon's rolls (the landing backpressure alert was raised
// four times in a day under a once-per-24-hours rule, gt-nhmh0w).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// alertClock persists when each named attention item was last raised. dir
// names the directory holding the entries; the zero value is not usable.
type alertClock struct {
	dir string
}

// last returns when key was last raised. A missing file, an unreadable file or
// a file that does not hold one RFC3339 line reads as never raised. The read
// has no error return because every caller only asks whether the alert window
// that started at the raise has passed: a forgotten raise costs one extra
// alert, while an error that suppressed the raise would hide the condition the
// clock exists to report.
func (c alertClock) last(key string) (time.Time, bool) {
	data, err := os.ReadFile(filepath.Join(c.dir, key))
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSuffix(string(data), "\n"))
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// stamp records at as when key was last raised. The directory is created at
// 0700 on first use and the entry is replaced atomically at 0600, so a reader
// never sees a partial line and a crash cannot leave a half-written instant.
func (c alertClock) stamp(key string, at time.Time) error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return fmt.Errorf("creating the alert clock directory: %w", err)
	}
	line := at.UTC().Format(time.RFC3339) + "\n"
	if err := atomicfile.WriteFile(filepath.Join(c.dir, key), []byte(line), 0o600); err != nil {
		return fmt.Errorf("writing the alert clock entry for %q: %w", key, err)
	}
	return nil
}

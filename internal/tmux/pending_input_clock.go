package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// How long input has been waiting (gt-afa7).
//
// The stall verdict is a conjunction: unsubmitted input is visible in the pane
// AND the session has produced no output for the whole frozen window. The
// second half reads #{window_activity}, which any write to the pane resets —
// so every nudge typed into a stalled composer refreshes the clock that would
// have caught it. This clock lives outside the pane and is immune to that.

// composerPendingSubdir is appended to the caller-supplied state directory.
// Callers pass <townRoot>/.runtime, putting these stamps beside the nudge
// queue's.
const composerPendingSubdir = "composer_pending"

// PendingInputClock tracks, per session, how long a composer has been
// continuously observed holding unsubmitted input.
//
// File-backed because its two callers are processes with different lifetimes —
// the daemon heartbeat and the witness patrol — and both must agree on when
// the wait started. A nil clock reports zero for every observation.
type PendingInputClock struct {
	dir string
}

// NewPendingInputClock returns a clock storing its stamps under
// <stateDir>/composer_pending, or nil when stateDir is empty. A clock with
// nowhere to persist is worse than none: the two callers would each measure
// their own wait and neither would reach the threshold.
func NewPendingInputClock(stateDir string) *PendingInputClock {
	if strings.TrimSpace(stateDir) == "" {
		return nil
	}
	return &PendingInputClock{dir: filepath.Join(stateDir, composerPendingSubdir)}
}

// Observe records that session was (pending true) or was not (pending false)
// holding unsubmitted input at now, and returns how long it has been
// continuously observed pending — zero on the first observation of a run, and
// zero after a non-pending one, so a later wait starts over rather than
// inheriting the old age.
//
// It reports no error: a clock that cannot be read or written must not change a
// stall verdict, so the worst case here is age zero, which falls back to the
// silence window. An unreadable stamp reads as "no run in progress".
func (c *PendingInputClock) Observe(session string, pending bool, now time.Time) time.Duration {
	if c == nil || strings.TrimSpace(session) == "" {
		return 0
	}
	path := c.path(session)
	if path == "" {
		return 0
	}

	if !pending {
		_ = os.Remove(path)
		return 0
	}

	if first, ok := c.read(path); ok {
		if age := now.Sub(first); age > 0 {
			return age
		}
		return 0
	}

	c.write(path, now)
	return 0
}

// Reset forgets session's pending run, for a caller that has just acted on the
// input: the wait it measured was unattended, and it no longer is.
func (c *PendingInputClock) Reset(session string) {
	if c == nil || strings.TrimSpace(session) == "" {
		return
	}
	if path := c.path(session); path != "" {
		_ = os.Remove(path)
	}
}

// path returns the stamp file for a session, or "" when the session name
// cannot be made into a filename.
func (c *PendingInputClock) path(session string) string {
	name := strings.NewReplacer("/", "_", string(filepath.Separator), "_").Replace(session)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return filepath.Join(c.dir, name)
}

// read returns the recorded start of session's pending run. Unix seconds, the
// same representation GetWindowActivity returns, so both clocks read alike in
// a log line.
func (c *PendingInputClock) read(path string) (time.Time, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

// write stores the start of session's pending run, best effort — a clock that
// cannot write reports age zero, leaving the caller on the silence window.
func (c *PendingInputClock) write(path string, now time.Time) {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return
	}
	stamp := []byte(strconv.FormatInt(now.Unix(), 10))
	// Rename so a concurrent reader never sees a partially-written stamp.
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, stamp, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

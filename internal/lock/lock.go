// Package lock provides agent identity locking to prevent multiple agents
// from claiming the same worker identity.
//
// Lock files are stored at <worker>/.runtime/agent.lock and contain:
// - PID of the owning process, plus the kernel's start token for it
// - Timestamp when lock was acquired
// - Session ID (tmux session name)
//
// A lock is stale only on evidence that the process which wrote it is gone: no
// process has the pid, or the pid is live under a start token other than the
// recorded one, which is what a reader sees once the writer died and the kernel
// handed its number to a stranger (gt-xa65s). Stale locks are automatically
// cleaned up.
package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/procid"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Common errors
var (
	ErrLocked      = errors.New("worker is locked by another agent")
	ErrNotLocked   = errors.New("worker is not locked")
	ErrInvalidLock = errors.New("invalid lock file")
)

// LockInfo contains information about who holds a lock.
type LockInfo struct {
	PID        int       `json:"pid"`
	AcquiredAt time.Time `json:"acquired_at"`
	SessionID  string    `json:"session_id,omitempty"`
	Hostname   string    `json:"hostname,omitempty"`
	// Start is the kernel's start token for PID (procid.StartToken): a pid is
	// reused once its process dies, so the pair names one process for good.
	// Empty in locks written before it was recorded, and on platforms whose
	// kernel will not say.
	Start string `json:"start,omitempty"`
}

// processTable reads the process table for one pid: alive reports whether any
// process has the pid, and token is the start token that process carries,
// empty when the kernel will not say.
type processTable func(pid int) (token string, alive bool)

// liveProcess is the processTable a Lock reads outside tests. Liveness and the
// token are separate reads because procid.StartToken answers ok=false both for
// a pid with no process and for a kernel that will not say, and only the first
// of those is evidence a holder is gone.
func liveProcess(pid int) (string, bool) {
	if !processExists(pid) {
		return "", false
	}
	token, ok := procid.StartToken(pid)
	if !ok {
		return "", true
	}
	return token, true
}

// IsStale checks if the lock is stale (the process that wrote it is gone).
func (l *LockInfo) IsStale() bool {
	held, _ := l.holder(liveProcess)
	return !held
}

// StaleReason names the evidence a stale lock was judged on, for the reports
// that list one; it is empty when the lock is not stale.
func (l *LockInfo) StaleReason() string {
	held, gone := l.holder(liveProcess)
	if held {
		return ""
	}
	return gone
}

// holderGone reports whether the process the lock names is gone, with the
// process table read through table.
func (l *LockInfo) holderGone(table processTable) bool {
	held, _ := l.holder(table)
	return !held
}

// holder is the staleness rule: held reports whether the process the lock
// names may still hold it, and gone names the evidence against it when it
// does not.
//
// Only evidence that the writer is gone makes a lock stale, because calling a
// live holder gone hands a running agent's identity to a second one. The
// evidence is a pid with no process, or a pid that carries a start token other
// than the recorded one — a pid whose process died and whose number the kernel
// reused, the case a bare pid reads as a live holder (gt-xa65s). A lock that
// recorded no token is that pid rule from before the token existed: its live
// pid keeps the lock, since nothing in it can be disproved.
func (l *LockInfo) holder(table processTable) (held bool, gone string) {
	if l.PID <= 0 {
		return false, fmt.Sprintf("PID %d is not a process number", l.PID)
	}
	token, alive := table(l.PID)
	if !alive {
		return false, fmt.Sprintf("dead PID %d", l.PID)
	}
	if l.Start == "" || token == "" || token == l.Start {
		return true, ""
	}
	return false, fmt.Sprintf("PID %d now names another process", l.PID)
}

// Lock represents an agent identity lock for a worker directory.
type Lock struct {
	workerDir string
	lockPath  string
	// table reads the process table; the field is what lets a test decide
	// staleness from what the lock recorded rather than from which processes
	// this host happens to be running.
	table processTable
}

// New creates a Lock for the given worker directory.
func New(workerDir string) *Lock {
	return &Lock{
		workerDir: workerDir,
		lockPath:  filepath.Join(workerDir, ".runtime", "agent.lock"),
		table:     liveProcess,
	}
}

// Acquire attempts to acquire the lock for this worker.
// Returns ErrLocked if another live process holds the lock.
// Automatically cleans up stale locks.
//
// Uses OS-level advisory locking (flock) to prevent TOCTOU races
// where two processes could both see no lock and both write one.
func (l *Lock) Acquire(sessionID string) error {
	// Ensure .runtime directory exists before flock
	dir := filepath.Dir(l.lockPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating lock directory: %w", err)
	}

	// Acquire advisory flock to serialize concurrent Acquire() calls.
	// The flock file is separate from the lock file itself.
	unlock, err := flockAcquire(l.lockPath + ".flock")
	if err != nil {
		return fmt.Errorf("acquiring coordination lock: %w", err)
	}
	defer unlock()

	// Check for existing lock (now safe from races)
	info, err := l.Read()
	if err == nil {
		// Lock exists - check if stale
		if info.holderGone(l.table) {
			// Stale lock - remove it
			if err := l.Release(); err != nil {
				return fmt.Errorf("removing stale lock: %w", err)
			}
		} else {
			// Active lock - check if it's us
			if info.PID == os.Getpid() {
				// We already hold it - refresh
				return l.write(sessionID)
			}
			// Another process holds it
			return fmt.Errorf("%w: PID %d (session: %s, acquired: %s)",
				ErrLocked, info.PID, info.SessionID, info.AcquiredAt.Format(time.RFC3339))
		}
	}

	// No lock or stale lock removed - acquire it
	return l.write(sessionID)
}

// Release releases the lock if we hold it.
func (l *Lock) Release() error {
	if err := os.Remove(l.lockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing lock file: %w", err)
	}
	return nil
}

// sameHolder reports whether b records the same write as a: a lock a peer
// rewrote since a was read differs in at least the acquisition time.
func (a *LockInfo) sameHolder(b *LockInfo) bool {
	return a.PID == b.PID && a.Start == b.Start && a.SessionID == b.SessionID &&
		a.Hostname == b.Hostname && a.AcquiredAt.Equal(b.AcquiredAt)
}

// reclaimStale removes the lock file that seen was read from and judged stale,
// provided it still holds seen. seen was read without the flock Acquire holds,
// so by now a concurrent Acquire may have removed the stale lock and written a
// live one, and an unconditional remove would delete that live lock and leave
// one identity with two agents. The file is therefore read again under the
// flock, and removed only if it is unchanged.
//
// removed reports that this call removed the file. fresh is the lock now on
// disk when it differs from seen, for the caller to judge afresh; it is nil
// when the file was removed or was already gone.
func (l *Lock) reclaimStale(seen *LockInfo) (fresh *LockInfo, removed bool, err error) {
	unlock, err := flockAcquire(l.lockPath + ".flock")
	if err != nil {
		return nil, false, fmt.Errorf("acquiring coordination lock: %w", err)
	}
	defer unlock()

	cur, err := l.Read()
	if errors.Is(err, ErrNotLocked) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !seen.sameHolder(cur) {
		return cur, false, nil
	}
	if err := l.Release(); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// Read reads the current lock info without modifying it.
func (l *Lock) Read() (*LockInfo, error) {
	data, err := os.ReadFile(l.lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotLocked
		}
		return nil, fmt.Errorf("reading lock file: %w", err)
	}

	var info LockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidLock, err)
	}

	return &info, nil
}

// Check checks if the worker is locked by another agent.
// Returns nil if unlocked or locked by us.
// Returns ErrLocked if locked by another live process.
// Automatically cleans up stale locks; a stale lock that cannot be examined
// or removed is returned as an error rather than read as "not locked".
func (l *Lock) Check() error {
	info, err := l.Read()
	if err != nil {
		if errors.Is(err, ErrNotLocked) {
			return nil // Not locked
		}
		return err
	}

	// Check if stale. A lock a concurrent Acquire rewrote while we were
	// deciding comes back from reclaimStale to be judged afresh.
	for info.holderGone(l.table) {
		fresh, _, err := l.reclaimStale(info)
		if err != nil {
			return fmt.Errorf("cleaning up stale lock: %w", err)
		}
		if fresh == nil {
			return nil // removed, or already gone
		}
		info = fresh
	}

	// Check if it's us
	if info.PID == os.Getpid() {
		return nil
	}

	// Locked by another process
	return fmt.Errorf("%w: PID %d (session: %s)", ErrLocked, info.PID, info.SessionID)
}

// Status returns a human-readable status of the lock.
func (l *Lock) Status() string {
	info, err := l.Read()
	if err != nil {
		if errors.Is(err, ErrNotLocked) {
			return "unlocked"
		}
		return fmt.Sprintf("error: %v", err)
	}

	// The evidence goes on screen with the verdict, because a dead pid and a
	// reused one are different repairs for the reader.
	if held, gone := info.holder(l.table); !held {
		return "stale (" + gone + ")"
	}

	if info.PID == os.Getpid() {
		return "locked (by us)"
	}

	return fmt.Sprintf("locked by PID %d (session: %s)", info.PID, info.SessionID)
}

// ForceRelease removes the lock regardless of who holds it.
// Use with caution - only for `gt doctor fix identity-collision` scenarios.
func (l *Lock) ForceRelease() error {
	return l.Release()
}

// write creates or updates the lock file.
func (l *Lock) write(sessionID string) error {
	// Ensure .runtime directory exists
	dir := filepath.Dir(l.lockPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating lock directory: %w", err)
	}

	hostname, _ := os.Hostname()
	info := LockInfo{
		PID:        os.Getpid(),
		AcquiredAt: time.Now(),
		SessionID:  sessionID,
		Hostname:   hostname,
	}
	// The start token is what lets a later reader tell this process from
	// whichever process the kernel hands the pid to next (gt-xa65s).
	if token, alive := l.table(os.Getpid()); alive && token != "" {
		info.Start = token
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling lock info: %w", err)
	}

	// Write to temp file then rename atomically to prevent partial lock files
	// on crash. os.Rename is atomic on POSIX when src and dst are on the same
	// filesystem (guaranteed here — same directory).
	tmpPath := l.lockPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil { //nolint:gosec // G306: lock files are non-sensitive operational data
		return fmt.Errorf("writing temp lock file: %w", err)
	}
	if err := os.Rename(tmpPath, l.lockPath); err != nil {
		// Clean up temp file on rename failure
		_ = os.Remove(tmpPath)
		return fmt.Errorf("renaming lock file: %w", err)
	}

	return nil
}

// FindAllLocks scans for agent.lock files, skipping large internal
// directories (.dolt-data, .git) that are extremely slow to walk on
// Docker bind mounts (macOS VirtioFS).
// Returns a map of worker directory -> LockInfo.
func FindAllLocks(root string) (map[string]*LockInfo, error) {
	locks := make(map[string]*LockInfo)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		// Skip large internal directories that never contain agent locks.
		if info.IsDir() {
			name := info.Name()
			if name == ".dolt-data" || name == ".dolt-backup" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}

		if filepath.Base(path) == "agent.lock" && filepath.Base(filepath.Dir(path)) == ".runtime" {
			workerDir := filepath.Dir(filepath.Dir(path))
			lock := New(workerDir)
			lockInfo, err := lock.Read()
			if err == nil {
				locks[workerDir] = lockInfo
			}
		}

		return nil
	})

	return locks, err
}

// CleanStaleLocks removes all stale locks in a directory tree.
// Returns the number of stale locks cleaned.
// A lock is only truly stale if BOTH its writer is gone AND the tmux session
// doesn't exist. This prevents killing active workers whose spawning process
// has exited (which is normal - Claude runs as a child in tmux).
func CleanStaleLocks(root string) (int, error) {
	return cleanStaleLocks(root,
		func() ([]string, error) {
			return activeTmuxSessions(realOutput, tmux.GetDefaultSocket())
		},
		liveProcess)
}

// cleanStaleLocks is CleanStaleLocks asking sessions for the active tmux
// sessions and table for the process table, once the lock scan has succeeded.
// A failed session listing reclaims nothing: a session missing from a listing
// that failed proves nothing about the worker.
func cleanStaleLocks(root string, sessions func() ([]string, error), table processTable) (int, error) {
	locks, err := FindAllLocks(root)
	if err != nil {
		return 0, err
	}

	// Get active tmux sessions to verify locks
	activeSessions, err := sessions()
	if err != nil {
		return 0, err
	}
	sessionSet := make(map[string]bool)
	for _, s := range activeSessions {
		sessionSet[s] = true
	}

	cleaned := 0
	var failed []error
	for workerDir, info := range locks {
		if info.holderGone(table) {
			// Writer is gone, but check if session still exists
			if info.SessionID != "" && sessionSet[info.SessionID] {
				// Session exists - worker is alive, don't clean
				continue
			}
			// Both PID dead AND no session = truly stale
			// The scan and the session list are both older than the flock,
			// so the removal re-checks the lock under it.
			lock := New(workerDir)
			lock.table = table
			fresh, removed, err := lock.reclaimStale(info)
			switch {
			case err != nil:
				// One worker the sweep cannot repair does not stop it
				// repairing the rest; the failure is reported to the caller.
				failed = append(failed, fmt.Errorf("removing stale lock in %s: %w", workerDir, err))
			case fresh != nil:
				// A concurrent Acquire rewrote the stale lock with its own:
				// that lock is live, so the sweep leaves it standing.
			case removed:
				cleaned++
			}
		}
	}

	return cleaned, errors.Join(failed...)
}

// activeTmuxSessions returns a list of active tmux session identifiers on the
// tmux server at socket ("" for tmux's default), listing them through run.
// Returns both session names (gt-foo-bar) and session IDs in various formats
// (%N, $N) to handle different lock file formats. A server that is not
// running has no sessions; any other failure to list is an error.
func activeTmuxSessions(run outputFunc, socket string) ([]string, error) {
	// Get both session name and ID to handle different lock formats
	// Format: "session_name:session_id" e.g., "gt-beads-crew-dave:$55"
	// Use the town's tmux socket so we query the correct server.
	// Without -L, bare "tmux" queries the "default" socket, which misses
	// all sessions on the per-town socket (e.g., "gt-a1b2c3") and causes
	// CleanStaleLocks to incorrectly remove locks for active sessions.
	args := []string{}
	if socket != "" {
		args = append(args, "-L", socket)
	}
	args = append(args, "list-sessions", "-F", "#{session_name}:#{session_id}")
	output, err := run("tmux", args...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && tmux.IsNoServerStderr(string(exitErr.Stderr)) {
			return nil, nil // no tmux server running: no sessions
		}
		return nil, fmt.Errorf("tmux list-sessions: %w", err)
	}

	var sessions []string
	for _, line := range splitLines(string(output)) {
		if line == "" {
			continue
		}
		// Parse "name:$id" format
		parts := splitOnColon(line)
		if len(parts) >= 1 {
			sessions = append(sessions, parts[0]) // session name
		}
		if len(parts) >= 2 {
			id := parts[1]
			sessions = append(sessions, id) // $N format
			// Also add %N format (old tmux style) for compatibility
			if len(id) > 0 && id[0] == '$' {
				sessions = append(sessions, "%"+id[1:])
			}
		}
	}
	return sessions, nil
}

// splitOnColon splits on the first colon only (session names shouldn't have colons)
func splitOnColon(s string) []string {
	idx := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return []string{s}
	}
	return []string{s[:idx], s[idx+1:]}
}

// outputFunc runs a program and returns its stdout.
type outputFunc func(name string, args ...string) ([]byte, error)

// realOutput runs name in its own process group and returns its stdout.
func realOutput(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // G204: command args are controlled internally
	setProcessGroup(cmd)
	return cmd.Output()
}

// splitLines splits a string into lines, handling both \n and \r\n
func splitLines(s string) []string {
	var lines []string
	var current []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, string(current))
			current = nil
		} else if s[i] == '\r' {
			// Skip \r
		} else {
			current = append(current, s[i])
		}
	}
	if len(current) > 0 {
		lines = append(lines, string(current))
	}
	return lines
}

// DetectCollisions finds workers with multiple agents claiming the same identity.
// This detects the case where multiple processes think they own the same worker
// by comparing tmux sessions with lock files.
// Returns a list of collision descriptions.
func DetectCollisions(root string, activeSessions []string) []string {
	return detectCollisions(root, activeSessions, liveProcess)
}

// detectCollisions is DetectCollisions reading the process table through table
// (liveProcess outside tests).
func detectCollisions(root string, activeSessions []string, table processTable) []string {
	var collisions []string

	locks, err := FindAllLocks(root)
	if err != nil {
		return collisions
	}

	// Build set of active sessions
	activeSet := make(map[string]bool)
	for _, s := range activeSessions {
		activeSet[s] = true
	}

	for workerDir, info := range locks {
		if held, gone := info.holder(table); !held {
			collisions = append(collisions,
				fmt.Sprintf("stale lock in %s (%s, session: %s)",
					workerDir, gone, info.SessionID))
			continue
		}

		// Check if the session in the lock matches an active session
		if info.SessionID != "" && !activeSet[info.SessionID] {
			collisions = append(collisions,
				fmt.Sprintf("orphaned lock in %s (session %s not found, PID %d still alive)",
					workerDir, info.SessionID, info.PID))
		}
	}

	return collisions
}

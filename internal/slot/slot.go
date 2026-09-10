// Package slot implements the town-level container-suite gate slot.
//
// The host's Docker VM is provisioned with a fixed CPU/memory bound
// (evidence: 12 vCPU / 8092 MiB, see gt-bcsq). Two Docker-backed test suites
// running concurrently inside it (e.g. beads' Dolt containers and gastown's
// testcontainers-based patrol tests) starve each other even when the HOST
// shows plenty of idle CPU, because host-idle only measures outside the VM.
// The slot ensures only one container-backed suite runs at a time townwide.
//
// Correctness comes from a kernel-managed advisory lock (flock(2)) on a
// persistent lock file, held by the process that outlives the suite and
// performs the release (see internal/lock.FlockAcquire /
// internal/lock.FlockTryAcquire). The kernel releases the lock automatically
// when the holding process dies by any means, including SIGKILL, so there is
// no stale-lock detection, no reclaim path, and no release race to get wrong
// — see the extensive discussion on gt-bcsq of why every hand-rolled
// mkdir/rm-based alternative has a check-then-act TOCTOU window.
//
// A separate "owner" metadata file is written alongside the lock purely for
// human/gt-status display (holder role, pid, acquired-at). It is NOT
// authoritative — Status() determines whether the slot is actually held by
// attempting a non-blocking flock, and only reads the owner file to decorate
// that result.
package slot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/lock"
)

// DefaultPollInterval is how often Acquire retries after a failed
// non-blocking attempt while waiting for a timed acquire.
const DefaultPollInterval = 2 * time.Second

// LockDir returns the directory holding the container-gate lock and owner
// files, creating it if necessary is the caller's responsibility (Acquire
// does this).
func LockDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "locks")
}

// LockPath returns the path to the flock-managed lock file itself. Its
// content is never inspected — only its lock state matters.
func LockPath(townRoot string) string {
	return filepath.Join(LockDir(townRoot), "container-gate.lock")
}

// OwnerPath returns the path to the display-only owner metadata file.
func OwnerPath(townRoot string) string {
	return filepath.Join(LockDir(townRoot), "container-gate.owner")
}

// Owner describes who currently (or most recently) held the slot. It is
// informational only — see package doc.
type Owner struct {
	Role       string    `json:"role"`
	PID        int       `json:"pid"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// Handle represents a held slot. Call Release exactly once when the
// container-backed suite has finished.
type Handle struct {
	townRoot string
	unlock   func()
}

// Acquire blocks until the container-gate slot is available (or timeout
// elapses) and then holds it via an OS advisory lock. A timeout <= 0 means
// wait indefinitely.
//
// role is a short human-readable identifier for the caller (e.g.
// "gastown/refinery" or a rig/MR id) and is recorded in the owner file for
// `gt status` / `gt doctor` display only; it plays no part in correctness.
func Acquire(townRoot, role string, timeout time.Duration) (*Handle, error) {
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}

	lockPath := LockPath(townRoot)

	unlock, err := acquireWithTimeout(lockPath, timeout)
	if err != nil {
		return nil, err
	}

	h := &Handle{townRoot: townRoot, unlock: unlock}

	// Best-effort: the owner file is display-only, so a write failure here
	// must not fail the acquire (we already hold the real lock).
	_ = atomicfile.EnsureDirAndWriteJSON(OwnerPath(townRoot), Owner{
		Role:       role,
		PID:        os.Getpid(),
		AcquiredAt: time.Now(),
	})

	return h, nil
}

// acquireWithTimeout polls FlockTryAcquire until it succeeds or timeout
// elapses, falling back to a single blocking FlockAcquire when timeout <= 0.
func acquireWithTimeout(lockPath string, timeout time.Duration) (func(), error) {
	if timeout <= 0 {
		return lock.FlockAcquire(lockPath)
	}

	deadline := time.Now().Add(timeout)
	for {
		unlock, ok, err := lock.FlockTryAcquire(lockPath)
		if err != nil {
			return nil, err
		}
		if ok {
			return unlock, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for container-gate slot", timeout)
		}
		time.Sleep(DefaultPollInterval)
	}
}

// Release releases the slot: the owner file is removed first (best-effort,
// display-only), then the advisory lock itself. The kernel would also
// release the lock on process exit/death even if Release is never called —
// this just makes a graceful release immediate instead of exit-triggered.
func (h *Handle) Release() error {
	_ = os.Remove(OwnerPath(h.townRoot))
	h.unlock()
	return nil
}

// Status reports whether the slot is currently held, decorated with owner
// metadata when available. The held/free determination itself comes from a
// non-blocking flock attempt on the real lock file — never from the owner
// file — so it can never report "free" while a live holder exists or
// "held" once the kernel has released the lock.
func Status(townRoot string) (held bool, owner *Owner, err error) {
	lockPath := LockPath(townRoot)
	if _, statErr := os.Stat(lockPath); os.IsNotExist(statErr) {
		return false, nil, nil
	}

	unlock, ok, err := lock.FlockTryAcquire(lockPath)
	if err != nil {
		return false, nil, err
	}
	if ok {
		// Nobody holds it: we just did, briefly. Release immediately and
		// clean up any leftover owner file from the last holder.
		unlock()
		_ = os.Remove(OwnerPath(townRoot))
		return false, nil, nil
	}

	// Held by someone else. The owner file is best-effort decoration.
	owner = readOwner(townRoot)
	return true, owner, nil
}

func readOwner(townRoot string) *Owner {
	data, err := os.ReadFile(OwnerPath(townRoot))
	if err != nil {
		return nil
	}
	var o Owner
	if err := json.Unmarshal(data, &o); err != nil {
		return nil
	}
	return &o
}

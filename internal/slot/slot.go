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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/lock"
)

// DefaultPollInterval is how often Acquire retries after a failed
// non-blocking attempt while waiting for a timed acquire.
const DefaultPollInterval = 2 * time.Second

// gateContainerPatterns matches the container images/names this gate cares
// about: Dolt's own server image, the testcontainers library's images, and
// its "ryuk" reaper sidecar. Substring match against `docker ps` output is
// deliberate (not a label filter) — gt-bcsq's evidence was that container
// COUNT is the authoritative "suite running" signal, and a filter narrower
// than reality (e.g. requiring a label suites don't all set) is exactly the
// kind of detector that missed a live suite before.
var gateContainerPatterns = []string{"dolt", "testcontainers", "ryuk"}

// runningGateContainers lists "<image> <names>" for every currently running
// Docker container whose image or name matches gateContainerPatterns. A
// non-nil error means the check could not be performed (docker daemon
// unreachable) — callers must treat that as "unknown", never as "no
// containers running". Declared as a var so tests can substitute a fake
// docker CLI response.
var runningGateContainers = func() ([]string, error) {
	out, err := exec.Command("docker", "ps", "--format", "{{.Image}} {{.Names}}").Output() //nolint:gosec // G204: fixed args, no user input
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			// docker binary itself is missing — this host can never run a
			// container-backed suite, so there is nothing to detect. This is
			// distinct from an *exec.ExitError (docker installed but the
			// daemon is unreachable), which IS treated as unknown below.
			return nil, nil
		}
		return nil, err
	}
	var matches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		for _, pat := range gateContainerPatterns {
			if strings.Contains(lower, pat) {
				matches = append(matches, line)
				break
			}
		}
	}
	return matches, nil
}

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
// The flock alone is not sufficient: a caller that starts a container-backed
// suite WITHOUT going through gt slot run leaves the flock untouched while
// still occupying the Docker VM (gt-tuiy). So a successful flock acquire is
// re-verified against `docker ps`: if matching containers are already
// running, or the docker daemon can't be reached to check, Acquire releases
// the flock and keeps waiting rather than handing out a slot that isn't
// actually safe to use.
//
// role is a short human-readable identifier for the caller (e.g.
// "gastown/refinery" or a rig/MR id) and is recorded in the owner file for
// `gt status` / `gt doctor` display only; it plays no part in correctness.
func Acquire(townRoot, role string, timeout time.Duration) (*Handle, error) {
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}

	lockPath := LockPath(townRoot)
	hasDeadline := timeout > 0
	deadline := time.Now().Add(timeout)

	for {
		unlock, ok, err := lock.FlockTryAcquire(lockPath)
		if err != nil {
			return nil, err
		}
		if ok {
			containers, containerErr := runningGateContainers()
			if containerErr == nil && len(containers) == 0 {
				h := &Handle{townRoot: townRoot, unlock: unlock}
				// Best-effort: the owner file is display-only, so a write
				// failure here must not fail the acquire (we already hold
				// the real lock).
				_ = atomicfile.EnsureDirAndWriteJSON(OwnerPath(townRoot), Owner{
					Role:       role,
					PID:        os.Getpid(),
					AcquiredAt: time.Now(),
				})
				return h, nil
			}
			// Either an unwrapped suite already has containers up, or we
			// can't tell (daemon unreachable) — neither is safe to hand the
			// slot out for, so release and keep waiting.
			unlock()
		}
		if hasDeadline && time.Now().After(deadline) {
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

// Report is the resolved state of the container-gate slot, combining the
// flock with a live `docker ps` check. Held alone is not the whole picture
// (gt-tuiy): a suite that started containers without going through gt slot
// run leaves the flock untouched, and a docker daemon that can't be reached
// means the check itself is inconclusive — neither case may be reported as
// free.
type Report struct {
	// Held is true when the flock is currently held by a gt slot run
	// holder. Owner is populated (best-effort) when Held is true.
	Held  bool
	Owner *Owner

	// UnwrappedContainers lists matching `docker ps` entries found while
	// Held is false — i.e. a container-backed suite is running without
	// holding the slot token. Always empty when Held is true (those
	// containers belong to the holder, not an "unwrapped" suite).
	UnwrappedContainers []string

	// DockerUnknown is true when the docker daemon could not be reached to
	// check for running containers, so UnwrappedContainers could not be
	// determined and must not be treated as "none found".
	DockerUnknown bool
}

// Busy reports whether the slot should be treated as unavailable: held by a
// token holder, occupied by an unwrapped container suite, or unverifiable
// because the docker daemon is unreachable.
func (r Report) Busy() bool {
	return r.Held || len(r.UnwrappedContainers) > 0 || r.DockerUnknown
}

// Status reports whether the slot is currently held, decorated with owner
// metadata when available, and cross-checks `docker ps` for a
// container-backed suite running without holding the token. The held/free
// determination itself comes from a non-blocking flock attempt on the real
// lock file — never from the owner file — so it can never report "free"
// while a live holder exists or "held" once the kernel has released the
// lock.
func Status(townRoot string) (Report, error) {
	var rep Report

	lockPath := LockPath(townRoot)
	if _, statErr := os.Stat(lockPath); statErr == nil {
		unlock, ok, err := lock.FlockTryAcquire(lockPath)
		if err != nil {
			return Report{}, err
		}
		if ok {
			// Nobody holds it: we just did, briefly. Release immediately and
			// clean up any leftover owner file from the last holder.
			unlock()
			_ = os.Remove(OwnerPath(townRoot))
		} else {
			// Held by someone else. The owner file is best-effort decoration.
			rep.Held = true
			rep.Owner = readOwner(townRoot)
		}
	}

	if !rep.Held {
		containers, err := runningGateContainers()
		if err != nil {
			rep.DockerUnknown = true
		} else {
			rep.UnwrappedContainers = containers
		}
	}

	return rep, nil
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

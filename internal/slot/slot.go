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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/lock"
)

// DefaultPollInterval is how often Acquire retries after a failed
// non-blocking attempt while waiting for a timed acquire.
const DefaultPollInterval = 2 * time.Second

// dockerPSTimeout bounds the `docker ps` call runningGateContainers makes.
// Status() is on gt-status's hot path (gt-tuiy: polled frequently by
// witness patrols and `gt slot status`), so a wedged docker daemon must not
// be able to hang it indefinitely — a bounded, explicit "unknown" beats an
// unbounded call that never returns.
const dockerPSTimeout = 5 * time.Second

// reentrantEnvVar marks, in a process's own environment, "<lockPath>|<pid>"
// for a container-gate slot it already holds — the lock path so a nested
// Acquire against a *different* slot still contends normally, and the PID
// so mutual exclusion is preserved between two unrelated callers that
// happen to share a process (e.g. two goroutines in the same test binary,
// or any future long-lived server): the reentrant fast path only applies
// when the caller's own PID differs from the recorded holder's, i.e. it is
// truly a descendant process that inherited the marker (`gt slot run`
// spawns children with the current environment by default), not a sibling
// call in the same process pretending to be one.
//
// This closes a real deadlock risk (gt-tuiy): the polecat-work formula's
// `gt slot run` wrap can run nested inside a Go call path that already
// holds the slot in-process (e.g. runMQBatchRun in mq_batch.go) via a
// spawned subprocess. Without reentrancy, that nested acquire would block
// forever on a flock its own ancestor process is still holding while
// waiting for the child to exit.
//
// Deliberately NOT prefixed GT_/BD_/BEADS_ (gt-tuiy attempt 4, CRITICAL):
// internal/testutil's hermetic test harness scrubs every variable with
// those prefixes from a test binary's environment before running any test
// (see internal/testutil/hermetic.go's scrubProcessEnv), specifically so a
// polecat session's ambient GT_*/BD_* vars can't leak into test
// subprocesses. A reentrant child spawned BY a hermetic test binary (e.g.
// internal/cmd's mq_batch reentrancy test) re-runs that same package's
// TestMain and gets scrubbed before its own test body ever sees this
// marker — a GT_-prefixed name would make the reentrant fast path
// untestable across that boundary even though it fires for real in
// production.
const reentrantEnvVar = "GASTOWN_SLOT_HELD"

// reentrantHolder parses reentrantEnvVar's value, returning the recorded
// lock path and PID.
func reentrantHolder() (lockPath string, pid int, ok bool) {
	val := os.Getenv(reentrantEnvVar)
	path, pidStr, found := strings.Cut(val, "|")
	if !found {
		return "", 0, false
	}
	p, err := strconv.Atoi(pidStr)
	if err != nil {
		return "", 0, false
	}
	return path, p, true
}

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
// unreachable, wedged, or refused the connection for some other reason) —
// callers must treat that as "unknown", never as "no containers running",
// except the specific isDaemonUnreachable case Acquire distinguishes (see
// its doc comment). Declared as a var so tests can substitute a fake docker
// CLI response.
var runningGateContainers = func() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerPSTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Image}} {{.Names}}").Output() //nolint:gosec // G204: fixed args, no user input
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			// docker binary itself is missing — this host can never run a
			// container-backed suite, so there is nothing to detect. This is
			// distinct from an *exec.ExitError (docker installed but the
			// daemon is unreachable), which IS treated as unknown below.
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("docker ps did not respond within %s: %w", dockerPSTimeout, ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			// Fold the docker CLI's own stderr into the error text so
			// isDaemonUnreachable can tell "daemon refused the connection"
			// (safe to infer nothing is running) apart from other exit
			// failures like a permission-denied socket (not safe to infer
			// anything) — exec.ExitError.Error() alone is just "exit status
			// N" and loses that distinction.
			return nil, fmt.Errorf("docker ps failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return matchGateContainers(string(out)), nil
}

// isDaemonUnreachable reports whether a runningGateContainers error means
// the docker daemon itself refused the connection (the CLI's own message
// for "not running" and "connection refused" — Docker emits the same
// wording for both), as opposed to a wedged daemon (docker ps timed out,
// see dockerPSTimeout), a permission-denied socket, or some other failure.
// Only the daemon-refused-connection case licenses "nothing could be
// running" — the others mean the check was inconclusive, not that it came
// back empty (om-editorial attempt 3 minor finding: "unreachable daemon ⇒
// no containers" is false for a wedged daemon or a permission-denied
// socket).
func isDaemonUnreachable(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "cannot connect to the docker daemon")
}

// SetContainerListerForTest overrides the function Acquire/Status use to
// list running gate containers, for tests in OTHER packages that cannot
// reach the unexported runningGateContainers var directly the way this
// package's own tests do (see stubNoContainers in slot_test.go).
// internal/cmd's batch-slot tests drive Acquire through
// acquireBatchGateSlot and must never shell out to the real docker CLI: a
// stray dolt/testcontainers/ryuk container on a shared Gas Town host would
// otherwise make Acquire poll for the full batchSlotTimeout and hang the
// whole test binary (gt-tuiy attempt 4, CRITICAL). Returns a restore func
// the caller must invoke (typically via t.Cleanup) to put the real lister
// back — the override is process-wide state shared by every test in the
// binary.
func SetContainerListerForTest(fn func() ([]string, error)) (restore func()) {
	prev := runningGateContainers
	runningGateContainers = fn
	return func() { runningGateContainers = prev }
}

// matchGateContainers filters raw `docker ps --format {{.Image}} {{.Names}}`
// output down to the lines matching gateContainerPatterns. Split out from
// runningGateContainers so the parsing/matching logic is testable without
// stubbing the docker CLI call itself.
func matchGateContainers(psOutput string) []string {
	var matches []string
	for _, line := range strings.Split(strings.TrimSpace(psOutput), "\n") {
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
	return matches
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
	townRoot  string
	unlock    func()
	reentrant bool // true: this Handle rides an ancestor's real hold; Release is a no-op.
}

// Acquire blocks until the container-gate slot is available (or timeout
// elapses) and then holds it via an OS advisory lock. A timeout <= 0 means
// wait indefinitely.
//
// The flock alone is not sufficient: a caller that starts a container-backed
// suite WITHOUT going through gt slot run leaves the flock untouched while
// still occupying the Docker VM (gt-tuiy). So a successful flock acquire is
// re-verified against `docker ps`: if matching containers are already
// running, Acquire releases the flock and keeps waiting rather than handing
// out a slot that isn't actually safe to use. If the docker daemon refused
// the connection, Acquire proceeds on the flock alone instead of failing or
// waiting: a daemon nothing can reach can't be running any containers
// either, so there is nothing left to verify (gt-tuiy attempt 2 — the
// earlier fail-fast behavior turned a stopped Docker Desktop into a
// town-wide gate outage even for suites that never touch Docker). Any other
// docker-ps failure (a wedged daemon that timed out, a permission-denied
// socket) is inconclusive rather than verified-empty, so it is treated like
// a real unwrapped container: release and keep waiting (see
// isDaemonUnreachable).
//
// role is a short human-readable identifier for the caller (e.g.
// "gastown/refinery" or a rig/MR id) and is recorded in the owner file for
// `gt status` / `gt doctor` display only; it plays no part in correctness.
func Acquire(townRoot, role string, timeout time.Duration) (*Handle, error) {
	lockPath := LockPath(townRoot)

	// Reentrant fast path: a *descendant* process of one that already holds
	// this exact slot does not compete with its own ancestor — see
	// reentrantEnvVar's doc comment. The PID check is what keeps this from
	// also matching an unrelated second caller sharing the same process
	// (e.g. TestAcquire_MutualExclusion's two calls from one goroutine, or
	// any future concurrent caller): only a genuinely different process
	// that inherited the marker takes this path.
	if holderPath, holderPID, ok := reentrantHolder(); ok && holderPath == lockPath && holderPID != os.Getpid() {
		return &Handle{townRoot: townRoot, unlock: func() {}, reentrant: true}, nil
	}

	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}

	hasDeadline := timeout > 0
	deadline := time.Now().Add(timeout)

	// grant hands out the slot: it records display-only owner metadata and
	// the reentrant marker (both best-effort; the write failing must not
	// fail the acquire, since the real lock is already held), then returns
	// the Handle. Shared by the two success paths below (clear docker check,
	// daemon-refused-connection) so they can't drift out of sync.
	grant := func(unlock func()) *Handle {
		h := &Handle{townRoot: townRoot, unlock: unlock}
		_ = atomicfile.EnsureDirAndWriteJSON(OwnerPath(townRoot), Owner{
			Role:       role,
			PID:        os.Getpid(),
			AcquiredAt: time.Now(),
		})
		_ = os.Setenv(reentrantEnvVar, lockPath+"|"+strconv.Itoa(os.Getpid()))
		return h
	}

	for {
		unlock, ok, err := lock.FlockTryAcquire(lockPath)
		if err != nil {
			return nil, err
		}
		if ok {
			containers, containerErr := runningGateContainers()
			switch {
			case containerErr != nil && !isDaemonUnreachable(containerErr):
				// Inconclusive (a wedged daemon that timed out, a
				// permission-denied socket, or some other exec failure) —
				// this does NOT license "nothing could be running" the way
				// isDaemonUnreachable does. Release and keep waiting exactly
				// as if a real unwrapped container were found.
				unlock()
			case containerErr != nil:
				// The daemon refused the connection: no Docker container can
				// be running against it either, so there is nothing an
				// unwrapped suite could be occupying — the flock we already
				// hold is sufficient on its own. Proceed rather than fail:
				// om-editorial (attempt 2, gt-tuiy) correctly called out
				// that failing here turns "Docker Desktop isn't running" —
				// routine on a dev box — into a town-wide outage for every
				// gate, including ones that never touch Docker. Status still
				// reports DockerUnknown/Busy for display; that's a distinct,
				// more conservative concern from Acquire's "is it safe to
				// hand out the slot" question.
				fmt.Fprintf(os.Stderr, "gt slot: docker daemon unreachable (%v); proceeding on flock alone\n", containerErr)
				return grant(unlock), nil
			case len(containers) == 0:
				return grant(unlock), nil
			default:
				// An unwrapped suite already has containers up — not safe to
				// hand the slot out for, so release and keep waiting.
				unlock()
			}
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
	if h.reentrant {
		// The real holder is an ancestor in this process tree; it owns the
		// owner file, the flock, and clearing reentrantEnvVar.
		return nil
	}
	_ = os.Remove(OwnerPath(h.townRoot))
	h.unlock()
	_ = os.Unsetenv(reentrantEnvVar)
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
	if _, statErr := os.Stat(lockPath); statErr == nil || !os.IsNotExist(statErr) {
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

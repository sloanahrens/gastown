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
//
// Waiting on that lock used to be invisible, which left the town unable to
// answer whether the gate was constricting it (gt-dc81). Acquire now measures
// the wait and puts it on Handle.WaitedFor, emits a slot_wait event per grant
// and a slot_hold per release, and writes both to a bounded ring file that
// `gt slot status` reports. See telemetry.go.
package slot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

// ReentrantEnvVar marks, in a process's own environment,
// "<lockPath>|<pid>|<role>" for a container-gate slot one of its ancestors
// holds — the lock path so a nested Acquire against a *different* slot
// still contends normally, the PID so mutual exclusion is preserved between
// two unrelated callers that happen to share a process (e.g. two goroutines
// in the same test binary, or any future long-lived server): the reentrant
// fast path only applies when the caller's own PID differs from the
// recorded holder's, i.e. it is truly a descendant process that inherited
// the marker (`gt slot run` spawns children with the current environment by
// default), not a sibling call in the same process pretending to be one.
//
// This closes a real deadlock risk (gt-tuiy): the polecat-work formula's
// `gt slot run` wrap can run nested inside a Go call path that already
// holds the slot in-process (e.g. runMQBatchRun in mq_batch.go) via a
// spawned subprocess. Without reentrancy, that nested acquire would block
// forever on a flock its own ancestor process is still holding while
// waiting for the child to exit.
//
// The trailing role scopes that fast path to the holder's own work
// (gt-off9). A marker is copied into a child's environment at fork(2) and
// the holder cannot clear it afterwards, so presence alone meant "anything
// descended from a holder skips the lock": every agent session, dog and
// plugin the daemon spawned while its main_branch_test hold was active
// carries the marker for the rest of its own life, and the refinery's gate,
// `gt done`'s verify suite and any `gt slot run` wrapper underneath them ran
// invisibly — no flock, no `docker ps` check, no owner file — straight past
// a hold that is supposed to serialize exactly that suite. Matching the role
// keeps reentrancy for the holder's own nested work while anything claiming
// different work contends for the real lock and queues fairly.
//
// The cost of that narrowing is deliberate: a nested acquire that names
// DIFFERENT work now waits for the holder to release instead of skipping
// its lock. It is a wait, not a failure: the caller prints "Waiting for
// container-gate slot ..." and reports slot contention, which is the
// outcome a lock is supposed to produce for unrelated work. A caller that
// legitimately nests inside a hold must pass that hold's role to stay
// reentrant — but the exact role string is an internal detail assembled by
// whichever Go call path took the ancestor hold (acquireBatchGateSlot,
// acquireMainBranchTestSlot, ...), not something a nested formula wrap or
// Makefile target can know in advance. `gt slot run` resolves an omitted
// --role to InheritedRole's answer before falling back to the
// per-invocation "pid-<pid>" placeholder, so the common case — a nested
// wrap that never names a role at all — rides its true ancestor's hold
// instead of reintroducing the gt-tuiy deadlock class against it. Nothing
// changes for a caller that DOES pass an explicit role naming different
// work: it still contends, exactly as designed.
//
// A marker written before roles were recorded is just "<lockPath>|<pid>"
// (an older gt binary holding the slot mid-rollover). It parses with an
// empty role and keeps the old presence-only behavior: a marker that cannot
// be role-checked must not start contending with the very ancestor that
// wrote it — that contention is the deadlock this variable exists to
// avoid.
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
//
// Exported, unlike the rest of this file's internals, because the marker is
// a wire format between processes: gt slot run's children, the daemon's
// gate commands, and the tests that exercise inheritance across a real
// fork all have to spell it.
const ReentrantEnvVar = "GASTOWN_SLOT_HELD"

// reentrantMark is a parsed ReentrantEnvVar value.
type reentrantMark struct {
	lockPath string
	pid      int
	// role is the role the holding process acquired under, or "" for a
	// legacy marker written before roles were recorded (see ReentrantEnvVar).
	role string
}

// parseReentrantMark parses a ReentrantEnvVar value.
func parseReentrantMark(val string) (reentrantMark, bool) {
	lockPath, rest, found := strings.Cut(val, "|")
	if !found {
		return reentrantMark{}, false
	}
	pidStr, role, _ := strings.Cut(rest, "|") // no role: legacy 2-field marker
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return reentrantMark{}, false
	}
	return reentrantMark{lockPath: lockPath, pid: pid, role: role}, true
}

// reentrantHolder returns the marker this process inherited, if any.
func reentrantHolder() (reentrantMark, bool) {
	return parseReentrantMark(os.Getenv(ReentrantEnvVar))
}

// validAncestor reports whether this marker names a genuine ancestor hold
// on townRoot's slot — as opposed to this process's own (or a sibling
// goroutine's) hold, or a marker naming some other town entirely. It is the
// townRoot-scoped half of grants, split out so InheritedRole can reuse it
// without also requiring a caller to already know the role to compare
// against.
func (m reentrantMark) validAncestor(townRoot string) bool {
	// A marker naming THIS process is its own (or a sibling goroutine's)
	// hold, not an ancestor's: it must still contend, or mutual exclusion
	// would not survive two callers in one process (gt-tuiy).
	if m.pid == os.Getpid() {
		return false
	}
	// A path outside this town's lock directory is somebody else's slot.
	_, ok := slotIndexFromLockPath(townRoot, m.lockPath)
	return ok
}

// grants reports whether this marker licenses a caller acquiring role in
// townRoot to take the reentrant fast path instead of locking for real.
func (m reentrantMark) grants(townRoot, role string) bool {
	if !m.validAncestor(townRoot) {
		return false
	}
	// Legacy markers predate the role field and keep the old permissive
	// reading; otherwise the caller has to be doing the same work.
	return m.role == "" || m.role == role
}

// InheritedRole returns the role of a genuine ancestor hold this process
// inherited on townRoot's container-gate slot, if any. It is what a nested
// wrap should pass as its own --role to ride that hold's reentrant fast
// path instead of contending against the very ancestor it is nested under
// (see ReentrantEnvVar and gt-off9) — the role-scoping that fast path now
// requires is otherwise a contract a caller has no way to discharge, since
// the ancestor's exact role string ("<rig>/refinery-batch",
// "<rig>/main-branch-test", ...) is assembled deep in Go call paths a
// shell-level nested wrap never sees. The second return is false when this
// process holds no marker, the marker names a different town, or the
// marker predates roles (an empty role carries no value to hand back, even
// though grants() still treats it as reentrant for any role).
func InheritedRole(townRoot string) (string, bool) {
	m, ok := reentrantHolder()
	if !ok || !m.validAncestor(townRoot) || m.role == "" {
		return "", false
	}
	return m.role, true
}

// reentrantEnvValue renders the marker a holder writes for its descendants.
func reentrantEnvValue(townRoot string, index int, role string, pid int) string {
	return SlotLockPath(townRoot, index) + "|" + strconv.Itoa(pid) + "|" + role
}

// gateContainerPatterns matches the container images/names this gate cares
// about: Dolt's own server image, the testcontainers library's images, and
// its "ryuk" reaper sidecar. Substring match against `docker ps` output is
// deliberate (not a label filter) — gt-bcsq's evidence was that container
// COUNT is the authoritative "suite running" signal, and a filter narrower
// than reality (e.g. requiring a label suites don't all set) is exactly the
// kind of detector that missed a live suite before.
var gateContainerPatterns = []string{"dolt", "testcontainers", "ryuk"}

// runningGateContainers lists the raw `docker ps` lines for every currently
// running Docker container whose image or name matches gateContainerPatterns.
// Each line is one of docker's own JSON records (see dockerPSFormat), which
// gateContainers parses into the age and session the gate classifies on.
//
// A non-nil error means the check could not be performed (docker daemon
// unreachable, wedged, or refused the connection for some other reason) —
// callers must treat that as "unknown", never as "no containers running",
// except the specific isDaemonUnreachable case Acquire distinguishes (see
// its doc comment). Declared as a var so tests can substitute a fake docker
// CLI response.
var runningGateContainers = func() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerPSTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", dockerPSFormat).Output() //nolint:gosec // G204: fixed args, no user input
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

// matchGateContainers filters raw `docker ps` output down to the lines for
// gate containers. Split out from runningGateContainers so the matching logic
// is testable without stubbing the docker CLI call itself.
func matchGateContainers(psOutput string) []string {
	var matches []string
	for _, line := range strings.Split(strings.TrimSpace(psOutput), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Matched on image and name, the two fields the old
		// "{{.Image}} {{.Names}}" listing carried. Matching the whole JSON
		// record instead would let a command or a label match, and gt-bcsq's
		// rule is that this detector stays no narrower than it was — not that
		// it widens into fields it never looked at.
		container := parseGateContainer(line)
		haystack := strings.ToLower(container.Image + " " + container.Name)
		for _, pat := range gateContainerPatterns {
			if strings.Contains(haystack, pat) {
				matches = append(matches, line)
				break
			}
		}
	}
	return matches
}

// gateContainers parses runningGateContainers' lines into the records the gate
// judges. Kept separate from the lister so the string seam the tests and the
// other rigs' packages stub stays what it was.
func gateContainers() ([]GateContainer, error) {
	lines, err := runningGateContainers()
	if err != nil {
		return nil, err
	}
	containers := make([]GateContainer, 0, len(lines))
	for _, line := range lines {
		containers = append(containers, parseGateContainer(line))
	}
	return containers, nil
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
	// Slot is the pool index this owner holds (0 for the original single
	// slot). Filled in by StatusPool when read back.
	Slot int `json:"slot"`
	// Name is the caller-given name of the marker this owner holds, written
	// only to a marker's owner file (see MarkerLockPath). A slot's owner file
	// leaves it empty: a slot is identified by its index.
	Name string `json:"name,omitempty"`
}

// Handle represents a held slot. Call Release exactly once when the
// container-backed suite has finished.
type Handle struct {
	townRoot  string
	unlock    func()
	reentrant bool // true: this Handle rides an ancestor's real hold; Release is a no-op.
	// Index is the pool slot this handle holds (0 for the single-slot case).
	Index int
	// WaitedFor is how long this call blocked before winning the slot: the wall
	// time from its first lock attempt to the grant (gt-dc81). It is 0 for a
	// slot won on the first attempt, and 0 for a reentrant handle, which holds
	// nothing of its own — the ancestor's wait is the one on record.
	WaitedFor time.Duration

	// role is the owner identity, kept for the telemetry Release reports under.
	role       string
	acquiredAt time.Time
	released   bool
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
// role is a short identifier for the caller (e.g. "gastown/refinery" or a
// rig/MR id). It is recorded in the owner file for `gt status` / `gt
// doctor` display, and — since gt-off9 — it also scopes the reentrant fast
// path: a marker only exempts a descendant doing the same role's work (see
// ReentrantEnvVar). Callers working on one item should therefore pass one
// stable role, not a per-invocation one.
func Acquire(townRoot, role string, timeout time.Duration) (*Handle, error) {
	return AcquirePool(townRoot, role, timeout, DefaultPool)
}

// Release releases the slot: the owner file is removed first (best-effort,
// display-only), then the advisory lock itself. The kernel would also
// release the lock on process exit/death even if Release is never called —
// this just makes a graceful release immediate instead of exit-triggered.
//
// The hold is recorded (slot_hold event and the ring file's held_s) without a
// command exit status; ReleaseWithExit is the variant for a holder that ran a
// command and knows how it ended.
func (h *Handle) Release() error { return h.release(nil) }

// ReleaseWithExit releases the slot, recording exitCode as the status of the
// command the slot was held for (gt-dc81).
//
// `gt slot run` needs this on its failure path: it exits with the child's code,
// and os.Exit skips deferred calls, so a plain deferred Release would never run
// and the ring file would keep that hold open forever.
func (h *Handle) ReleaseWithExit(exitCode int) error { return h.release(&exitCode) }

// release is Release and ReleaseWithExit's shared body. exitCode is nil when
// there is no command status to report.
func (h *Handle) release(exitCode *int) error {
	if h.reentrant {
		// The real holder is an ancestor in this process tree; it owns the
		// owner file, the flock, and clearing ReentrantEnvVar.
		return nil
	}
	if h.released {
		// release is called from a defer, and `gt slot run`'s failure path
		// calls ReleaseWithExit before the deferred Release would have run.
		// Recording the hold twice would double-count it in the history.
		return nil
	}
	h.released = true

	held := time.Since(h.acquiredAt)
	_ = os.Remove(SlotOwnerPath(h.townRoot, h.Index))
	h.unlock()
	_ = os.Unsetenv(ReentrantEnvVar)

	// Telemetry is best-effort and runs after the lock is gone: a caller
	// blocked on this slot must not be made to wait on a stats write, and a
	// grant is worth more than the record of it.
	if _, err := completeHold(h.townRoot, h.role, h.Index, os.Getpid(), held); err != nil {
		fmt.Fprintf(probeWriter, "gt slot: recording slot hold in history: %v\n", err)
	}
	emitHoldEvent(h.townRoot, h.role, h.Index, held, exitCode)
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
	//
	// Debris is not in here: a gate container past the staleness window with
	// no live reaper is not a suite anyone is waiting on (gt-ul1k), and
	// counting it as one is what let a single orphan block the town.
	UnwrappedContainers []string

	// DebrisContainers lists matching `docker ps` entries classified as
	// debris: old enough that no live suite can be behind them. Reported so
	// an operator can see what 'gt slot reap' would remove; never a reason to
	// call the slot busy.
	DebrisContainers []string

	// DockerUnknown is true when the docker daemon could not be reached to
	// check for running containers, so UnwrappedContainers could not be
	// determined and must not be treated as "none found".
	DockerUnknown bool

	// Slots is the per-slot picture when the report came from StatusPool or
	// StatusPoolLocksOnly (Status fills it with the single slot 0, as does
	// the locks-only path). HeldCount/Total summarize it; Reserved is how many
	// low slots only gate roles may take. In-flight markers follow the pool's
	// own slots as Marker rows, which Total and HeldCount do not count.
	Slots     []SlotState
	HeldCount int
	Total     int
	Reserved  int
}

// Busy reports whether a new suite could NOT be admitted right now: every
// slot held (a single slot: held at all), an unwrapped container suite
// occupying the Docker VM, or the docker check unverifiable. With a pool,
// "held" (Held / HeldCount) and "busy" therefore differ: one of three slots
// held is Held but not Busy. `gt slot status --json` exposes both, plus
// "saturated" as an explicit alias for the all-slots-held case.
func (r Report) Busy() bool {
	if r.Total > 0 {
		return r.AllHeld() || len(r.UnwrappedContainers) > 0 || r.DockerUnknown
	}
	return r.Held || len(r.UnwrappedContainers) > 0 || r.DockerUnknown
}

// Status reports whether the slot is currently held, decorated with owner
// metadata when available, and cross-checks `docker ps` for a
// container-backed suite running without holding the token. The held/free
// determination itself comes from a non-blocking flock attempt on the real
// lock file — never from the owner file — so it can never report "free"
// while a live holder exists or "held" once the kernel has released the
// lock.
//
// A caller that reads only Held/Owner should use StatusPoolLocksOnly and
// spare itself the `docker ps` shell-out (gt-a8kx).
func Status(townRoot string) (Report, error) {
	return StatusPool(townRoot, DefaultPool)
}

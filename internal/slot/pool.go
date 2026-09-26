package slot

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/lock"
)

// Pool describes how many container-gate slots the town hands out and how
// many of them are reserved for gate-class callers (the refinery, the batch
// gate, the daemon's main-branch test). One slot per concurrently-running
// container-backed suite: the Docker VM's fixed CPU/memory bound is the
// reason the count exists at all (see package doc), and the measured
// per-suite footprint (200-340 MiB of an 8 GiB VM, gt-xtk4) is why the count
// can be more than one.
//
// Slot 0 keeps the original single-slot lock file name, so a gt binary that
// predates the pool and holds "the" slot is, to a pooled binary, simply the
// holder of slot 0 — the two never hand out the same slot twice during an
// install rollover.
type Pool struct {
	// Slots is the total number of slots. Values < 1 are treated as 1.
	Slots int
	// ReservedForGate is how many of the lowest-numbered slots only
	// gate-class roles (IsGateRole) may take. Gate roles may also take any
	// unreserved slot; everyone else only competes for the unreserved
	// ones. Clamped so at least one unreserved slot always exists when
	// Slots > 1; with Slots == 1 nothing is reserved (reserving the only
	// slot would lock polecats out entirely).
	ReservedForGate int
}

// DefaultPool is the pre-pool behavior: one slot, nothing reserved. Acquire
// and Status use it.
var DefaultPool = Pool{Slots: 1}

// normalized returns the pool with the invariants in Pool's doc applied.
func (p Pool) normalized() Pool {
	if p.Slots < 1 {
		p.Slots = 1
	}
	if p.ReservedForGate < 0 {
		p.ReservedForGate = 0
	}
	if p.Slots == 1 {
		p.ReservedForGate = 0
	} else if p.ReservedForGate > p.Slots-1 {
		p.ReservedForGate = p.Slots - 1
	}
	return p
}

// gateRoleMarkers are the substrings that identify a gate-class caller in
// the role string recorded by every slot user: the refinery's own gate
// ("<rig>/refinery"), the batch gate ("<rig>/refinery-batch", mq_batch.go),
// and the daemon's periodic main-branch test ("<rig>/main-branch-test").
// Polecat roles are "<rig>/<polecat-name>" and never contain these.
var gateRoleMarkers = []string{"/refinery", "/main-branch-test"}

// IsGateRole reports whether role belongs to the gate class that may use
// reserved slots.
func IsGateRole(role string) bool {
	for _, m := range gateRoleMarkers {
		if strings.Contains(role, m) {
			return true
		}
	}
	return false
}

// candidates returns the slot indices role may take, in the order Acquire
// tries them: gate roles try the reserved slots first, then the shared
// ones; everyone else only the shared ones.
func (p Pool) candidates(role string) []int {
	p = p.normalized()
	var out []int
	if IsGateRole(role) {
		for i := 0; i < p.Slots; i++ {
			out = append(out, i)
		}
		return out
	}
	for i := p.ReservedForGate; i < p.Slots; i++ {
		out = append(out, i)
	}
	return out
}

// SlotLockPath returns the flock-managed lock file for slot i. Slot 0 is
// the original single-slot path (LockPath) for install-rollover safety.
func SlotLockPath(townRoot string, i int) string {
	if i == 0 {
		return LockPath(townRoot)
	}
	return filepath.Join(LockDir(townRoot), fmt.Sprintf("container-gate-%d.lock", i))
}

// SlotOwnerPath returns the display-only owner metadata file for slot i.
func SlotOwnerPath(townRoot string, i int) string {
	if i == 0 {
		return OwnerPath(townRoot)
	}
	return filepath.Join(LockDir(townRoot), fmt.Sprintf("container-gate-%d.owner", i))
}

// slotIndexFromLockPath is the inverse of SlotLockPath for paths inside
// townRoot's LockDir; ok is false for anything else.
func slotIndexFromLockPath(townRoot, path string) (int, bool) {
	if filepath.Dir(path) != LockDir(townRoot) {
		return 0, false
	}
	base := filepath.Base(path)
	if base == filepath.Base(LockPath(townRoot)) {
		return 0, true
	}
	if !strings.HasPrefix(base, "container-gate-") || !strings.HasSuffix(base, ".lock") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(base, "container-gate-"), ".lock"))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// discoverSlots returns every slot index that has a lock file on disk, so
// Status can report slots created by a larger pool than the caller knows
// about (a newer binary, a config change between invocations).
func discoverSlots(townRoot string) []int {
	entries, err := os.ReadDir(LockDir(townRoot))
	if err != nil {
		return nil
	}
	var idx []int
	for _, e := range entries {
		if i, ok := slotIndexFromLockPath(townRoot, filepath.Join(LockDir(townRoot), e.Name())); ok {
			idx = append(idx, i)
		}
	}
	sort.Ints(idx)
	return idx
}

// othersHeld reports how many slots other than exclude are currently held,
// by non-blocking flock probes on every known slot. Used by AcquirePool to
// decide whether running gate containers are somebody's legitimate suite
// (some other slot is held) or an unwrapped one (no slot held at all,
// gt-tuiy). A probe that fails to open counts as not held.
func othersHeld(townRoot string, pool Pool, exclude int) int {
	seen := map[int]bool{}
	var idx []int
	for i := 0; i < pool.normalized().Slots; i++ {
		seen[i] = true
		idx = append(idx, i)
	}
	for _, i := range discoverSlots(townRoot) {
		if !seen[i] {
			idx = append(idx, i)
		}
	}
	held := 0
	for _, i := range idx {
		if i == exclude {
			continue
		}
		path := SlotLockPath(townRoot, i)
		if _, statErr := os.Stat(path); statErr != nil {
			continue
		}
		unlock, ok, err := lock.FlockTryAcquire(path)
		if err != nil {
			continue
		}
		if ok {
			unlock()
			continue
		}
		held++
	}
	return held
}

// AcquirePool is Acquire over a pool of slots: it tries each slot the role
// is allowed to take (Pool.candidates) with a non-blocking flock and holds
// the first free one. All of Acquire's semantics carry over — kernel flock
// is the only source of truth, the owner file is decoration, a descendant
// of a holder doing the same role's work is reentrant — with one refinement
// to the gt-tuiy unwrapped-container check: running gate containers only
// block a grant when NO slot is held by anyone, because once any slot is
// held, containers are exactly what that holder is expected to be running.
//
// On success this process's environment is armed with ReentrantEnvVar so
// its descendants inherit the fast path. AcquirePoolReal is the same
// acquire for a caller that must be visible to everyone instead.
func AcquirePool(townRoot, role string, timeout time.Duration, pool Pool) (*Handle, error) {
	return acquirePool(townRoot, role, timeout, pool, false)
}

// AcquirePoolReal acquires a slot as a first-class holder: it always takes
// the real flock, writes the owner file and runs the `docker ps` check, and
// never takes the reentrant fast path, even when the caller's own role is
// the one a marker it inherited names.
//
// The daemon's main_branch_test runner uses this (gt-off9). The daemon
// outlives every hold it takes, so riding a marker is the wrong move for it:
// a marker naming that very role can be inherited from a predecessor daemon
// process — the kernel drops the predecessor's flock when it dies, but a
// marker it armed lives on in the environment of everything it spawned, so
// every one of the successor's cycles would report "acquired" and run
// invisibly, with no flock, no owner file and no `docker ps` check. That is
// exactly the hole this package exists to close.
//
// It arms ReentrantEnvVar like AcquirePool does, so a suite that nests its
// own `gt slot run` (or another in-process holder) inside this run keeps the
// fast path instead of deadlocking against the hold. The marker still
// reaches everything else the daemon spawns while the run is in flight —
// agent sessions, dogs and plugins, whose environments are fixed at spawn
// and cannot be repaired when the hold ends — which is why the marker names
// the holder's role: grants admits only a caller doing the same role's work,
// so their gate commands and verifies queue behind this hold rather than
// skipping its lock (gt-off9).
func AcquirePoolReal(townRoot, role string, timeout time.Duration, pool Pool) (*Handle, error) {
	return acquirePool(townRoot, role, timeout, pool, true)
}

// probeWriter is where the gate's `docker ps` diagnostics go — the counterpart
// to debrisWriter, which carries the verdicts on the containers Acquire walks
// past. Both an inconclusive probe and an unreachable daemon are reported
// here, so an operator has one stream to look at when the gate waits for a
// reason it cannot name. Declared as a var so a test can read the evidence
// back rather than have it land on its own stderr.
var probeWriter io.Writer = os.Stderr

// inconclusiveLogger returns a func that reports the first inconclusive
// `docker ps` probe it is shown, then stays quiet.
//
// Acquire polls while it waits, so an unverifiable probe would otherwise
// re-announce itself every DefaultPollInterval. But silence is worse here than
// for debris: the error this branch eventually returns is
// "timed out after %s waiting for container-gate slot", which names no cause,
// and for runMQBatchRun that timeout is 60 minutes — long enough that a
// wedged daemon was indistinguishable from a busy town (gt-a8kx). One line
// carrying the underlying error is the difference between a diagnosable wait
// and a mystery hang.
func inconclusiveLogger() func(error) {
	logged := false
	return func(err error) {
		if logged {
			return
		}
		logged = true
		fmt.Fprintf(probeWriter, "gt slot: docker ps check inconclusive (%v); cannot confirm no container-backed suite is running, waiting for the gate instead of handing out a slot\n", err)
	}
}

// waitLogger returns a func that reports why the first blocked pass of one
// Acquire call could not grant, then stays quiet.
//
// Acquire polls while it waits, so a cause announced on every pass would
// re-announce itself every DefaultPollInterval. Silence is the worse failure:
// the timeout the caller finally returns names no cause, and for
// runMQBatchRun that is up to batchSlotTimeout — an hour with nothing on the
// pane for the ordinary case of a pool whose slots are all held (gt-78b8). A
// wedged docker probe already names itself (inconclusiveLogger, gt-a8kx), so
// this reports the two remaining ways to be held up: every candidate slot
// held by a live process, and an unwrapped suite.
func waitLogger(role string, timeout time.Duration) func(waitInfo) {
	logged := false
	return func(info waitInfo) {
		if logged || info.Reason == "" || info.Reason == WaitReasonDaemonUnreachable {
			return
		}
		logged = true
		cap := ""
		if timeout > 0 {
			cap = fmt.Sprintf(" (cap %s)", timeout.Round(time.Second))
		}
		fmt.Fprintf(probeWriter, "gt slot: waiting for container-gate slot as %s — %s%s\n", role, info.describe(), cap)
	}
}

// acquirePool implements AcquirePool (firstClass false) and AcquirePoolReal
// (firstClass true).
func acquirePool(townRoot, role string, timeout time.Duration, pool Pool, firstClass bool) (*Handle, error) {
	pool = pool.normalized()

	// Reentrant fast path (see ReentrantEnvVar): a descendant of a process
	// holding ANY slot of this town, doing the same role's work, does not
	// compete with its ancestor. A first-class holder never takes it.
	if !firstClass {
		if m, ok := reentrantHolder(); ok && m.grants(townRoot, role) {
			return &Handle{townRoot: townRoot, unlock: func() {}, reentrant: true}, nil
		}
	}

	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}

	candidates := pool.candidates(role)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("container-gate pool has no slot available to role %q (slots=%d reserved=%d)", role, pool.Slots, pool.ReservedForGate)
	}

	hasDeadline := timeout > 0
	deadline := time.Now().Add(timeout)

	// Announced once per container per Acquire call: a town with one orphan
	// beside a busy suite would otherwise print the same warning on every
	// poll of the wait.
	logDebrisOnce := debrisLogger()

	// Likewise one removal attempt per orphan per Acquire call: a removal that
	// failed is logged once, not retried on every poll.
	removeOrphanOnce := orphanRemover()

	// Likewise once per Acquire call, and for the same reason: the wait can
	// outlast several polls.
	logInconclusiveOnce := inconclusiveLogger()

	// Likewise once per Acquire call: the cause of the wait, named as soon as
	// a pass is blocked (gt-78b8).
	logWaitOnce := waitLogger(role, timeout)

	// watch attributes the wait this call is about to spend (gt-dc81).
	watch := newWaitWatch()

	grant := func(i int, unlock func()) *Handle {
		info := watch.info(timeout, false)
		h := &Handle{
			townRoot:   townRoot,
			unlock:     unlock,
			Index:      i,
			role:       role,
			acquiredAt: time.Now(),
			WaitedFor:  info.Waited,
		}
		_ = atomicfile.EnsureDirAndWriteJSON(SlotOwnerPath(townRoot, i), Owner{
			Role:       role,
			PID:        os.Getpid(),
			AcquiredAt: time.Now(),
			Slot:       i,
		})
		// Both acquire paths arm the marker for their own descendants; only
		// the fast path differs between them (see AcquirePoolReal).
		_ = os.Setenv(ReentrantEnvVar, reentrantEnvValue(townRoot, i, role, os.Getpid()))
		if err := recordWaitResult(townRoot, role, i, os.Getpid(), info); err != nil {
			fmt.Fprintf(probeWriter, "gt slot: recording slot acquisition in history: %v\n", err)
		}
		emitWaitEvent(townRoot, role, i, info)
		return h
	}

	for {
		passStart := time.Now()
		// passReason is why this pass could not grant; "" means nothing
		// blocked it, which only a grant can end.
		var passReason WaitReason
		for _, i := range candidates {
			unlock, ok, err := lock.FlockTryAcquire(SlotLockPath(townRoot, i))
			if err != nil {
				return nil, err
			}
			if !ok {
				// Somebody holds this slot. Read their owner file now, while
				// they are still the blocker: by the time the wait ends they
				// have released and removed it (gt-dc81).
				watch.noteHolder(readSlotOwner(townRoot, i))
				if passReason == "" {
					passReason = WaitReasonTokenHeld
				}
				continue
			}
			// We hold slot i. If somebody else holds another slot, any
			// running gate containers are taken to be theirs and the
			// docker probe is skipped. Residual gap, accepted for pool
			// liveness: a holder running a container-free command beside
			// a third party's UNWRAPPED suite would not be detected here
			// (the single-slot check could not tell those apart either
			// once a slot was held). gt slot status still reports unwrapped
			// containers whenever no slot is held.
			if othersHeld(townRoot, pool, i) > 0 {
				return grant(i, unlock), nil
			}
			containers, containerErr := gateContainers()
			switch {
			case containerErr != nil && !isDaemonUnreachable(containerErr):
				// Inconclusive probe (wedged daemon, permission-denied
				// socket): not licensed to assume empty. Release and wait,
				// naming the underlying error the first time so the wait is
				// diagnosable rather than a silent spin to the timeout
				// (gt-a8kx).
				logInconclusiveOnce(containerErr)
				watch.noteDockerErr(containerErr)
				if passReason == "" {
					passReason = WaitReasonDaemonUnreachable
				}
				unlock()
			case containerErr != nil:
				fmt.Fprintf(probeWriter, "gt slot: docker daemon unreachable (%v); proceeding on flock alone\n", containerErr)
				return grant(i, unlock), nil
			default:
				var blocking []string
				for _, verdict := range Classify(containers, time.Now(), StaleContainerWindow) {
					if verdict.Blocks() {
						blocking = append(blocking, verdict.Container.Display())
						continue
					}
					logDebrisOnce(verdict)
					// A container whose labeled owner is certainly gone is
					// removed here rather than left for 'gt slot reap': it is
					// what a killed or failed suite leaves behind, and no live
					// process can be using it (gt-ehlga). Age-only debris
					// stays the reaper's call.
					if verdict.OwnerGone {
						removeOrphanOnce(verdict)
					}
				}
				if len(blocking) == 0 {
					return grant(i, unlock), nil
				}
				// An unwrapped suite has containers up and nobody holds a
				// slot for them. Not safe to hand out ANY slot; release
				// and wait rather than trying the next candidate.
				watch.noteContainers(blocking)
				if passReason == "" {
					passReason = WaitReasonUnwrappedContainers
				}
				unlock()
			}
			break
		}
		if passReason != "" {
			logWaitOnce(watch.blockedInfo(timeout, passReason))
		}
		if hasDeadline && time.Now().After(deadline) {
			// A caller that gave up is recorded like a grant. It is the
			// strongest evidence of a constricting gate and the case a
			// grant-only history drops: two `gt slot run` calls timed out at 30s
			// behind one unwrapped dolt suite on 2026-09-22 and left no trace
			// anywhere until this did (gt-dc81).
			info := watch.info(timeout, true)
			if err := recordWaitResult(townRoot, role, candidates[0], os.Getpid(), info); err != nil {
				fmt.Fprintf(probeWriter, "gt slot: recording slot timeout in history: %v\n", err)
			}
			emitWaitEvent(townRoot, role, candidates[0], info)
			return nil, fmt.Errorf("timed out after %s waiting for container-gate slot", timeout)
		}
		time.Sleep(DefaultPollInterval)
		// Credited after the sleep so a blocked pass carries the poll interval
		// it spent waiting, not just the microseconds its probes took.
		watch.credit(passReason, time.Since(passStart))
	}
}

// SlotState is the resolved state of one slot in a StatusPool report.
type SlotState struct {
	Index int    `json:"index"`
	Held  bool   `json:"held"`
	Owner *Owner `json:"owner,omitempty"`
	// Marker is set on a row reporting an in-flight marker rather than a pool
	// slot: held outside the pool's count, identified by Name, and numbered
	// after the pool so it collides with no slot index (see marker.go).
	Marker bool `json:"marker,omitempty"`
	// Name is the marker's caller-given name, empty on a pool slot's row.
	Name string `json:"name,omitempty"`
}

// StatusPool is Status over a pool: it reports every slot the pool defines
// plus any extra slot that exists on disk. Report.Held is true when ANY
// slot is held and Report.Owner is the lowest held slot's owner, so
// single-slot callers keep their meaning; Report.Slots carries the full
// picture. The docker cross-check for unwrapped suites runs only when no
// slot is held at all.
//
// That cross-check shells out to `docker ps` (bounded by dockerPSTimeout),
// which is the price a caller pays only when it is deciding whether a suite
// may start or reporting what the Docker VM is doing. A caller that reads
// nothing but the held/owner picture wants StatusPoolLocksOnly instead
// (gt-a8kx).
func StatusPool(townRoot string, pool Pool) (Report, error) {
	rep, err := StatusPoolLocksOnly(townRoot, pool)
	if err != nil {
		return Report{}, err
	}
	if rep.HeldCount > 0 {
		// Containers up while a slot is held belong to that holder, not to an
		// unwrapped suite, so there is nothing here to cross-check.
		return rep, nil
	}

	containers, err := gateContainers()
	if err != nil {
		rep.DockerUnknown = true
		return rep, nil
	}
	for _, verdict := range Classify(containers, time.Now(), StaleContainerWindow) {
		if verdict.Blocks() {
			rep.UnwrappedContainers = append(rep.UnwrappedContainers, verdict.Container.Display())
		} else {
			rep.DebrisContainers = append(rep.DebrisContainers, verdict.Container.Display())
		}
	}
	return rep, nil
}

// StatusPoolLocksOnly is StatusPool without the `docker ps` cross-check: the
// held/free picture and the owner metadata, read from the flocks and the
// display-only owner files alone. Both halves are in-process work — non-
// blocking flock probes plus small file reads, no subprocess — so it is safe
// on a path that runs on every refresh.
//
// UnwrappedContainers, DebrisContainers and DockerUnknown are therefore always
// zero-valued, so this report is indistinguishable from a full one by its
// fields alone. Do NOT read it as "no container-backed suite is running":
// nothing here looked, and treating an unchecked host as free is the exact
// collision this package exists to prevent (gt-tuiy). It answers "is the slot
// held, and by whom" — nothing more, and only for callers that read nothing
// more.
//
// gt status's gatherStatus (internal/cmd/status.go) and the polecat Stop
// hook's polecatStopVerificationRunning (internal/cmd/tap_polecat_stop.go) are
// the intended callers: both read only Held/Owner-and-slots, yet the full
// StatusPool made each of them shell out to docker ps whenever no slot was
// held (gt-a8kx). Callers that do need the container half — `gt slot status`,
// the web dashboard's Gate panel, the refinery's Busy() check, Acquire itself
// — keep using StatusPool/Status.
func StatusPoolLocksOnly(townRoot string, pool Pool) (Report, error) {
	pool = pool.normalized()
	var rep Report

	seen := map[int]bool{}
	var idx []int
	for i := 0; i < pool.Slots; i++ {
		seen[i] = true
		idx = append(idx, i)
	}
	for _, i := range discoverSlots(townRoot) {
		if !seen[i] {
			idx = append(idx, i)
		}
	}
	sort.Ints(idx)

	for _, i := range idx {
		st := SlotState{Index: i}
		path := SlotLockPath(townRoot, i)
		if _, statErr := os.Stat(path); statErr == nil || !os.IsNotExist(statErr) {
			unlock, ok, err := lock.FlockTryAcquire(path)
			if err != nil {
				return Report{}, err
			}
			if ok {
				unlock()
				_ = os.Remove(SlotOwnerPath(townRoot, i))
			} else {
				st.Held = true
				st.Owner = readSlotOwner(townRoot, i)
				rep.HeldCount++
				if !rep.Held {
					rep.Held = true
					rep.Owner = st.Owner
				}
			}
		}
		rep.Slots = append(rep.Slots, st)
	}
	rep.Total = len(rep.Slots)
	rep.Reserved = pool.ReservedForGate

	// Markers are appended after the pool's own rows and counted in neither
	// Total nor HeldCount: a review in flight holds no pool slot, and a pool
	// reported held — or saturated — on its account would misstate what may
	// start (gt-97cm). Their index continues the pool's numbering, so a marker
	// takes no index a pool slot could hold.
	poolRows := len(rep.Slots)
	for i, m := range liveMarkers(townRoot) {
		owner := m.owner
		if owner != nil {
			owner.Slot = poolRows + i
		}
		rep.Slots = append(rep.Slots, SlotState{
			Index:  poolRows + i,
			Held:   true,
			Marker: true,
			Name:   m.name,
			Owner:  owner,
		})
	}
	return rep, nil
}

// readSlotOwner reads slot i's display-only owner file, tagging it with the
// index it was read from.
func readSlotOwner(townRoot string, i int) *Owner {
	o := readOwnerFile(SlotOwnerPath(townRoot, i))
	if o == nil {
		return nil
	}
	o.Slot = i
	return o
}

// readOwnerFile parses one owner metadata file, returning nil when it is
// missing or unreadable.
func readOwnerFile(path string) *Owner {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var o Owner
	if err := json.Unmarshal(data, &o); err != nil {
		return nil
	}
	return &o
}

// HeldBy returns the slots and markers currently held by role, from a report.
func (r Report) HeldBy(role string) []SlotState {
	var out []SlotState
	for _, s := range r.Slots {
		if s.Held && s.Owner != nil && s.Owner.Role == role {
			out = append(out, s)
		}
	}
	return out
}

// AllHeld reports whether every slot the report knows about is held.
func (r Report) AllHeld() bool {
	return r.Total > 0 && r.HeldCount == r.Total
}

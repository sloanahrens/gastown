package slot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/procid"
)

// Pool describes how many container-gate slots the town hands out and how
// many of them are reserved for gate-class callers (the daemon's landing
// worker). One slot per concurrently-running
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
	// YieldToGate makes a NEW non-gate acquisition wait while any
	// gate-reserved slot is held by a live process, so a landing gate never
	// shares the machine with a crew or agent suite that started after it
	// (gt-22hdp.29: a 7-12 min gate took 24 min beside two crew make-test
	// runs). A holder that is already running is never preempted, and a gate
	// acquisition never yields. No effect when ReservedForGate is 0.
	YieldToGate bool
	// MaxGateYield bounds how long one acquisition yields to running gates
	// in total, so a hung gate or gates running back to back cannot starve
	// the rest of the town; after it the waiter competes for a shared slot
	// as before. Values <= 0 mean DefaultMaxGateYield.
	MaxGateYield time.Duration
	// MaxFullSuites caps how many full-suite-class holders (IsFullSuiteRole)
	// may run at once anywhere in the pool. A whole-tree test run is the
	// heaviest thing a seat starts, and several at once starve the landing
	// gate (gt-dhcmp: whole-suite runs from several seats at once pushed one
	// landing's gate to 5m02s against a 26-39s norm). A full-suite start at
	// the cap waits for a running one instead of competing for a free slot;
	// ordinary package-scoped suites are unaffected. Values <= 0 mean no cap.
	//
	// The cap never applies to a gate-class role, and a gate never counts
	// against it: the gate is the merge path's critical section and holds
	// priority (see Pool.ReservedForGate).
	MaxFullSuites int
}

// DefaultMaxGateYield is Pool.MaxGateYield's default: longer than a slow gate
// (24 min measured beside two crew suites, gt-22hdp.29), short enough that a
// wedged one costs the town one wait rather than the day.
const DefaultMaxGateYield = config.DefaultContainerGateMaxGateYield

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
	if p.MaxGateYield <= 0 {
		p.MaxGateYield = DefaultMaxGateYield
	}
	if p.Slots == 1 {
		p.ReservedForGate = 0
	} else if p.ReservedForGate > p.Slots-1 {
		p.ReservedForGate = p.Slots - 1
	}
	return p
}

// gateRoleSuffixes identify the gate-class callers in the role string
// recorded by every slot user: the daemon's landing worker's gate
// ("<rig>/landing") and its post-landing run ("<rig>/post-land"), which is
// the red-main detector (gt-v4ssj.4, gt-bhdk2), both in landing_worker.go.
// The refinery and main-branch-test roles that used to share the class are
// deleted (gt-v4ssj.6, gt-v4ssj.4).
var gateRoleSuffixes = []string{"/landing", "/post-land"}

// IsGateRole reports whether role belongs to the gate class that may use
// reserved slots and that non-gate acquisitions yield to.
func IsGateRole(role string) bool {
	return hasRoleSuffix(role, gateRoleSuffixes)
}

// fullSuiteRoleSuffixes identify the full-suite class: roles whose work is a
// whole-tree test run. The daemon's tier sweep ("<rig>/tier-sweep") and the
// flake sweep ("<rig>/flake-sweep") are the named ones; "/full-suite" is the
// suffix any other caller appends when the command it wraps is a whole-tree
// run (`gt slot run --role <rig>/<who>/full-suite -- make test`), which is how
// a crew or operator session puts its own suite under the cap.
var fullSuiteRoleSuffixes = []string{"/full-suite", "/tier-sweep", "/flake-sweep"}

// IsFullSuiteRole reports whether role belongs to the full-suite class: a
// whole-tree test run, which the pool caps at Pool.MaxFullSuites concurrent
// holders townwide. It is deliberately disjoint from IsGateRole — the landing
// gate runs the whole tree too, but it must never wait on this cap.
func IsFullSuiteRole(role string) bool {
	return hasRoleSuffix(role, fullSuiteRoleSuffixes)
}

// hasRoleSuffix reports whether any of suffixes is a suffix of role.
func hasRoleSuffix(role string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(role, suffix) {
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

// knownSlotIndices returns every slot index pool defines plus every extra slot
// that exists on disk, so a reader sees slots a larger pool or a newer binary
// created (discoverSlots) as well as its own.
func knownSlotIndices(townRoot string, pool Pool) []int {
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
	return idx
}

// writeSlotOwner publishes slot i's holder metadata the moment the flock is
// taken, before the acquisition does anything that can take time. The file is
// decoration for the held/free question — the flock is the truth — but two
// readers judge a hold by it, runningGate and liveFullSuiteHolders, and each
// would otherwise see the claimant as absent for as long as it sits in the
// `docker ps` cross-check (bounded by dockerPSTimeout). A write that fails is
// deliberately not fatal: the flock is still held, and the cost of a missing
// file is a full-suite cap that under-counts this holder (gt-dhcmp). It is also
// nearly unreachable — the file lands in the same directory as the lock file
// Acquire just created, so a directory an owner file cannot be written to is one
// the flock would have failed on first.
func writeSlotOwner(townRoot string, i int, role string, pid int, at time.Time) {
	start, _ := procid.StartToken(pid)
	_ = atomicfile.EnsureDirAndWriteJSON(SlotOwnerPath(townRoot, i), Owner{
		Role:       role,
		PID:        pid,
		Start:      start,
		AcquiredAt: at,
		Slot:       i,
	})
}

// settleSlotOwner republishes slot i's owner file once the acquisition is past
// its `docker ps` probe, marking the claim as a holder's rather than a
// claimant's (Owner.Settled). The claim is published at writeSlotOwner so the
// gate and the full-suite cap see a holder throughout the probe (gt-dhcmp);
// this second write is what tells a reader which of the two it is looking at.
//
// The distinction is load-bearing for othersHeld: a claimant still in its probe
// may yet release the slot, so it must not be what excuses another acquirer
// from probing for an unwrapped suite (gt-u0zq0). A missing or unreadable file
// is left alone — the claim already reached its readers as far as it can, and
// nothing here is fatal to the hold, which the flock is (writeSlotOwner).
func settleSlotOwner(townRoot string, i int) {
	owner := readSlotOwner(townRoot, i)
	if owner == nil || owner.Settled {
		return
	}
	owner.Settled = true
	_ = atomicfile.EnsureDirAndWriteJSON(SlotOwnerPath(townRoot, i), *owner)
}

// dropSlotClaim withdraws a claim writeSlotOwner published and releases the
// slot: a claimant that walks away from a slot must not leave an owner file
// behind for the gate or the full-suite cap to read as a live hold.
func dropSlotClaim(townRoot string, i int, unlock func()) {
	_ = os.Remove(SlotOwnerPath(townRoot, i))
	unlock()
}

// othersHeld reports how many slots other than exclude are held by a settled
// holder — one that has finished its acquisition probe and is running its work
// — by non-blocking flock probes on every known slot. Used by AcquirePool to
// decide whether running gate containers are somebody's legitimate suite
// (some other slot is held) or an unwrapped one (no slot held at all,
// gt-tuiy). A probe that fails to open counts as not held.
//
// A claimant that holds the flock but has not settled is not counted. It is
// still deciding whether to keep the slot, and counting it is how two
// simultaneous acquirers each concluded the other held a slot and both skipped
// the unwrapped-container probe, so an unwrapped suite could run beside them
// undetected (gt-u0zq0). With only settled holders counted, both probe, which
// is the safe side; each grants on a later pass once the other has settled. A
// held slot whose owner file is missing or unreadable counts as not settled for
// the same reason: an unreadable claim is not evidence of a suite already
// running.
//
// Residual gap, on the same side: an owner file written by a binary that
// predates Owner.Settled carries no flag and so reads as a claimant's, and a
// new acquirer meets an old holder across an install rollover by probing beside
// its suite and waiting for its containers rather than granting beside them.
// The cost is a wait, never a grant this check exists to prevent.
func othersHeld(townRoot string, pool Pool, exclude int) int {
	held := 0
	for _, i := range knownSlotIndices(townRoot, pool) {
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
		if owner := readSlotOwner(townRoot, i); owner == nil || !owner.Settled {
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
	return NewGate().AcquirePool(townRoot, role, timeout, pool)
}

// AcquirePool is the package-level AcquirePool on this gate.
func (g *Gate) AcquirePool(townRoot, role string, timeout time.Duration, pool Pool) (*Handle, error) {
	return g.acquirePool(townRoot, role, timeout, pool, false)
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
	return NewGate().AcquirePoolReal(townRoot, role, timeout, pool)
}

// AcquirePoolReal is the package-level AcquirePoolReal on this gate.
func (g *Gate) AcquirePoolReal(townRoot, role string, timeout time.Duration, pool Pool) (*Handle, error) {
	return g.acquirePool(townRoot, role, timeout, pool, true)
}

// inconclusiveLogger returns a func that reports the first inconclusive
// `docker ps` probe it is shown, then stays quiet.
//
// Acquire polls while it waits, so an unverifiable probe would otherwise
// re-announce itself every DefaultPollInterval. But silence is worse here than
// for debris: for runMQBatchRun the timeout runs to 60 minutes (gt-a8kx), so
// the underlying error has to reach the pane when the probe first fails rather
// than wait for the timeout error to name it an hour later (gt-18zj).
func inconclusiveLogger(w io.Writer) func(error) {
	logged := false
	return func(err error) {
		if logged {
			return
		}
		logged = true
		fmt.Fprintf(w, "gt slot: docker ps check inconclusive (%v); cannot confirm no container-backed suite is running, waiting for the gate instead of handing out a slot\n", err)
	}
}

// waitLogger returns a func that reports why the first blocked pass of one
// Acquire call could not grant, then stays quiet.
//
// Acquire polls while it waits, so a cause announced on every pass would
// re-announce itself every DefaultPollInterval. Silence is the worse failure:
// for runMQBatchRun the wait runs up to batchSlotTimeout — an hour with
// nothing on the pane for the ordinary case of a pool whose slots are all held
// (gt-78b8) — so the cause is named when it starts blocking, not only by the
// timeout error at the end (gt-18zj). A wedged docker probe already names
// itself (inconclusiveLogger, gt-a8kx), so this reports the two remaining ways
// to be held up: every candidate slot held by a live process, and an unwrapped
// suite.
func waitLogger(w io.Writer, role string, timeout time.Duration) func(waitInfo) {
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
		fmt.Fprintf(w, "gt slot: waiting for container-gate slot as %s — %s%s\n", role, info.describe(), cap)
	}
}

// acquirePool implements AcquirePool (firstClass false) and AcquirePoolReal
// (firstClass true).
func (g *Gate) acquirePool(townRoot, role string, timeout time.Duration, pool Pool, firstClass bool) (*Handle, error) {
	pool = pool.normalized()

	// Reentrant fast path (see ReentrantEnvVar): a descendant of a process
	// holding ANY slot of this town, doing the same role's work, does not
	// compete with its ancestor. A first-class holder never takes it.
	if !firstClass {
		if m, ok := g.reentrantHolder(townRoot); ok && m.grants(townRoot, role, g.pid) {
			return &Handle{gate: g, townRoot: townRoot, unlock: func() {}, reentrant: true}, nil
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
	deadline := g.clock.Now().Add(timeout)

	// Announced once per container per Acquire call: a town with one orphan
	// beside a busy suite would otherwise print the same warning on every
	// poll of the wait.
	logDebrisOnce := debrisLogger(g.debrisOut)

	// Likewise one removal attempt per orphan per Acquire call: a removal that
	// failed is logged once, not retried on every poll.
	removeOrphanOnce := orphanRemover(g.runtime, g.debrisOut)

	// Likewise once per Acquire call, and for the same reason: the wait can
	// outlast several polls.
	logInconclusiveOnce := inconclusiveLogger(g.probeOut)

	// Likewise once per Acquire call: the cause of the wait, named as soon as
	// a pass is blocked (gt-78b8).
	logWaitOnce := waitLogger(g.probeOut, role, timeout)

	// watch attributes the wait this call is about to spend (gt-dc81).
	watch := newWaitWatch(g.clock)

	grant := func(i int, unlock func()) *Handle {
		info := watch.info(timeout, false)
		h := &Handle{
			gate:       g,
			townRoot:   townRoot,
			unlock:     pinHold(unlock),
			Index:      i,
			role:       role,
			acquiredAt: g.clock.Now(),
			WaitedFor:  info.Waited,
		}
		// Both acquire paths arm the marker for their own descendants; only
		// the fast path differs between them (see AcquirePoolReal). The owner
		// file was written when the slot was claimed (writeSlotOwner), so this
		// package's readers saw this holder for the whole docker probe; it is
		// settled now, with the probe behind this grant, so a reader can tell
		// this holder from a claimant still deciding (othersHeld).
		settleSlotOwner(townRoot, i)
		armReentrant(g.env, townRoot, i, role, g.pid)
		if err := recordWaitResult(townRoot, role, i, g.pid, g.clock.Now(), info); err != nil {
			fmt.Fprintf(g.probeOut, "gt slot: recording slot acquisition in history: %v\n", err)
		}
		emitWaitEvent(g.probeOut, townRoot, role, i, info)
		return h
	}

	// Yielding to a running gate (gt-22hdp.29) applies to a non-gate caller
	// only, and not to work nested under a gate's own hold: yielding to the
	// very gate it runs inside would stall that gate on itself. yielded is
	// this call's total time spent yielding, bounded by MaxGateYield so no
	// gate — hung, leaked or merely followed by another — starves it.
	yieldApplies := pool.YieldToGate && pool.ReservedForGate > 0 && !IsGateRole(role) && !g.underGateHold(townRoot)

	// The full-suite cap (Pool.MaxFullSuites, gt-dhcmp): once that many
	// full-suite-class holders are live, a new one cedes rather than starting
	// another whole-tree run beside them. Unlike the gate yield it is not bounded
	// by MaxGateYield — a full-suite run is bounded work, and the caller's own
	// timeout is the backstop — and it never applies to a gate-class role.
	// Work nested under a full-suite hold of its own is exempt (see
	// underFullSuiteHold): capping it would stall that hold on itself.
	fullSuiteApplies := pool.MaxFullSuites > 0 && IsFullSuiteRole(role) && !g.underFullSuiteHold(townRoot)

	var yielded time.Duration
	yieldCapLogged := false

	for {
		passStart := g.clock.Now()
		// passReason is why this pass could not grant; "" means nothing
		// blocked it, which only a grant can end.
		var passReason WaitReason
		if yieldApplies && yielded < pool.MaxGateYield {
			if owner, running := g.runningGate(townRoot, pool); running {
				watch.noteGateHolder(owner)
				passReason = WaitReasonGateRunning
			}
		}
		if passReason == "" && fullSuiteApplies {
			if holders := g.liveFullSuiteHolders(townRoot, pool); len(holders) >= pool.MaxFullSuites {
				watch.noteFullSuiteHolder(holders[0])
				passReason = WaitReasonFullSuiteHeld
			}
		}
		for _, i := range candidates {
			// The two reasons set before the loop are decisions not to compete
			// for a slot this pass at all. Reasons a pass sets while walking
			// the candidates (a held token, an unwrapped suite) are not, so
			// the walk keeps going to the next candidate.
			if passReason == WaitReasonGateRunning || passReason == WaitReasonFullSuiteHeld {
				break
			}
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
			// We hold slot i — publish the claim before the docker probe
			// below can park this acquisition for up to dockerPSTimeout, so
			// the gate and the full-suite cap see this holder throughout
			// (writeSlotOwner).
			writeSlotOwner(townRoot, i, role, g.pid, g.clock.Now())
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
			containers, containerErr := g.gateContainers()
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
				dropSlotClaim(townRoot, i, unlock)
			case containerErr != nil:
				fmt.Fprintf(g.probeOut, "gt slot: docker daemon unreachable (%v); proceeding on flock alone\n", containerErr)
				return grant(i, unlock), nil
			default:
				var blocking []string
				for _, verdict := range g.owner.classify(containers, g.clock.Now(), StaleContainerWindow) {
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
				dropSlotClaim(townRoot, i, unlock)
			}
			break
		}
		if passReason != "" {
			logWaitOnce(watch.blockedInfo(timeout, passReason))
		}
		if hasDeadline && g.clock.Now().After(deadline) {
			// A caller that gave up is recorded like a grant. It is the
			// strongest evidence of a constricting gate and the case a
			// grant-only history drops: two `gt slot run` calls timed out at 30s
			// behind one unwrapped dolt suite on 2026-09-22 and left no trace
			// anywhere until this did (gt-dc81).
			info := watch.info(timeout, true)
			if err := recordWaitResult(townRoot, role, candidates[0], g.pid, g.clock.Now(), info); err != nil {
				fmt.Fprintf(g.probeOut, "gt slot: recording slot timeout in history: %v\n", err)
			}
			emitWaitEvent(g.probeOut, townRoot, role, candidates[0], info)
			return nil, timeoutError(timeout, info)
		}
		g.clock.Sleep(g.pollInterval)
		// Credited after the sleep so a blocked pass carries the poll interval
		// it spent waiting, not just the microseconds its probes took.
		passTime := g.clock.Since(passStart)
		watch.credit(passReason, passTime)
		if passReason == WaitReasonGateRunning {
			yielded += passTime
			if yielded >= pool.MaxGateYield && !yieldCapLogged {
				yieldCapLogged = true
				fmt.Fprintf(g.probeOut, "gt slot: %s yielded %s to running gates, reaching the yield cap %s; competing for a shared slot now\n",
					role, yielded.Round(time.Second), pool.MaxGateYield.Round(time.Second))
			}
		}
	}
}

// timeoutError is the error a caller that exhausted its cap returns. It carries
// the wait's cause — the one the ring file and the slot_wait event already
// record (gt-dc81) — because a reader of the failure alone has to tell a held
// pool from an unwrapped suite, a wedged docker probe and a running gate: four
// different fixes, and for runMQBatchRun a wait of up to an hour before the
// error surfaces at all (gt-18zj).
func timeoutError(timeout time.Duration, info waitInfo) error {
	msg := fmt.Sprintf("timed out after %s waiting for container-gate slot", timeout)
	if cause := info.describe(); cause != "" {
		msg += " — " + cause
	}
	return errors.New(msg)
}

// underGateHold reports whether this process runs under a gate hold that is
// still in force: it inherited a gate role's reentrant marker (see
// ReentrantEnvVar) and the slot that marker names is still held by the pid it
// names. A marker outlives its hold in every process spawned while the hold
// was active (gt-off9), so the marker alone is not enough.
func (g *Gate) underGateHold(townRoot string) bool {
	return g.underHoldOfClass(townRoot, IsGateRole)
}

// underFullSuiteHold is underGateHold for the full-suite class: this process
// runs under a live full-suite hold of its own. The cap is skipped for such a
// caller, because the holder the cap would make it wait for is the very
// ancestor it runs inside — a wait that could only end when that ancestor's
// own suite finishes, i.e. never (the gt-tuiy deadlock class, gt-off9's
// reentrancy contract).
func (g *Gate) underFullSuiteHold(townRoot string) bool {
	return g.underHoldOfClass(townRoot, IsFullSuiteRole)
}

// underHoldOfClass reports whether this process inherited a marker for a live
// ancestor hold whose role is in the class match selects.
func (g *Gate) underHoldOfClass(townRoot string, match func(string) bool) bool {
	m, ok := g.reentrantHolder(townRoot)
	if !ok || !m.validAncestor(townRoot, g.pid) || !match(m.role) {
		return false
	}
	i, _ := slotIndexFromLockPath(townRoot, m.lockPath)
	if _, err := os.Stat(m.lockPath); err != nil {
		// Probing would create the file, and a created slot file is a slot
		// Status then reports (discoverSlots).
		return false
	}
	unlock, free, err := lock.FlockTryAcquire(m.lockPath)
	if err != nil {
		return false
	}
	if free {
		unlock()
		return false
	}
	owner := readSlotOwner(townRoot, i)
	return owner != nil && owner.PID == m.pid
}

// runningGate reports whether a live merge gate (IsGateRole) holds one of
// pool's gate-reserved slots, and that gate's owner.
//
// It reads the slots' owner files and checks the owner's pid, and never
// probes the flock: a probe takes the lock for an instant, and a gate whose
// own FlockTryAcquire landed in that instant would fall through to a shared
// slot. The owner file is written right after the flock is taken and removed
// right before it is released, so a live pid in it is a live hold. A holder
// that died without releasing leaves a pid that is gone, the same death the
// kernel drops the flock for; its file is ignored. A slot whose owner file is
// not written yet is not waited on for that pass.
func (g *Gate) runningGate(townRoot string, pool Pool) (*Owner, bool) {
	pool = pool.normalized()
	for i := 0; i < pool.ReservedForGate; i++ {
		owner := readSlotOwner(townRoot, i)
		if owner == nil || !IsGateRole(owner.Role) || owner.PID <= 0 || g.ownerGone(owner) {
			continue
		}
		return owner, true
	}
	return nil, false
}

// liveFullSuiteHolders returns the owners of every live full-suite-class holder
// in pool. AcquirePool's full-suite cap compares the count against
// Pool.MaxFullSuites to decide whether a new one may start.
//
// It reads the owner files and checks the owner's pid, exactly as runningGate
// does and for the same reasons: a probe would take the lock for an instant,
// and a full-suite holder acquiring in that window would fall through; the
// owner file is written right after the flock is taken and removed right
// before it is released, so a live pid in it is a live hold, while a holder
// that died without releasing leaves a pid that is gone.
//
// Every known slot is read, not just the pool's: a full-suite holder may hold
// any slot it was allowed to take, including one a larger earlier pool left on
// disk (knownSlotIndices).
//
// Residual gap, accepted for the same reason AcquirePool accepts its own: a
// holder between its flock and its owner-file write (writeSlotOwner, called
// with no work in between) is invisible here, so two full-suite starts landing
// in that instant can both win the cap. The window is one syscall and one small
// file write — not the docker probe, which the claim is published ahead of —
// and its cost is one extra whole-tree run, not starvation: this is a load
// bound, not a mutual-exclusion contract, and every later start sees the extra
// holder. The alternative, a second lock file to make the cap exact, is the
// separate lock the bead this cap implements rules out.
func (g *Gate) liveFullSuiteHolders(townRoot string, pool Pool) []*Owner {
	var holders []*Owner
	for _, i := range knownSlotIndices(townRoot, pool) {
		owner := readSlotOwner(townRoot, i)
		if owner == nil || !IsFullSuiteRole(owner.Role) || owner.PID <= 0 || g.ownerGone(owner) {
			continue
		}
		holders = append(holders, owner)
	}
	return holders
}

// ownerGone reports whether owner names a process that certainly no longer
// exists. Unknown owners are not gone.
//
// A pid is reused once its process dies, so a live process at the owner's pid
// is not the same as a live owner: when the file recorded the start token the
// pid had when it was written (Owner.Start) and the process at that pid now
// carries a different one, this is a stranger wearing the owner's pid and the
// owner is gone. The check is skipped when either side cannot be read — an
// unreadable start time leaves the pid as the only evidence, which is what
// every reader had before the token existed (gt-u0zq0).
func (g *Gate) ownerGone(owner *Owner) bool {
	if owner == nil || owner.PID <= 0 {
		return false
	}
	if g.owner.gone != nil && g.owner.gone(owner.PID) {
		return true
	}
	if owner.Start == "" || g.owner.startToken == nil {
		return false
	}
	current, ok := g.owner.startToken(owner.PID)
	return ok && current != owner.Start
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
	return NewGate().StatusPool(townRoot, pool)
}

// StatusPool is the package-level StatusPool on this gate.
func (g *Gate) StatusPool(townRoot string, pool Pool) (Report, error) {
	rep, err := g.StatusPoolLocksOnly(townRoot, pool)
	if err != nil {
		return Report{}, err
	}
	if rep.HeldCount > 0 {
		// Containers up while a slot is held belong to that holder, not to an
		// unwrapped suite, so there is nothing here to cross-check.
		return rep, nil
	}

	containers, err := g.gateContainers()
	if err != nil {
		rep.DockerUnknown = true
		return rep, nil
	}
	for _, verdict := range g.owner.classify(containers, g.clock.Now(), StaleContainerWindow) {
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
	return NewGate().StatusPoolLocksOnly(townRoot, pool)
}

// StatusPoolLocksOnly is the package-level StatusPoolLocksOnly on this gate.
func (g *Gate) StatusPoolLocksOnly(townRoot string, pool Pool) (Report, error) {
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
				// Remove the stale owner file while the flock is still held: an
				// unlock first would let the next acquirer take the slot and
				// write its own owner file in the window before the remove,
				// which this then deletes — the new holder's file, gone
				// (gt-u0zq0). A slot's file only ever belongs to its holder, so
				// a removal under the lock removes only a dead holder's.
				_ = os.Remove(SlotOwnerPath(townRoot, i))
				unlock()
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

	// The yield state reads the rows just probed rather than probing again:
	// a held reserved slot whose owner is still alive is a running gate (see
	// runningGate).
	if pool.YieldToGate {
		for _, st := range rep.Slots {
			if st.Index < pool.ReservedForGate && st.Held && st.Owner != nil && IsGateRole(st.Owner.Role) && !g.ownerGone(st.Owner) {
				rep.YieldingToGate = true
				rep.GateHolder = st.Owner
				break
			}
		}
	}

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

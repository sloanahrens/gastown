package slot

import (
	"encoding/json"
	"fmt"
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
// of a holder is reentrant — with one refinement to the gt-tuiy
// unwrapped-container check: running gate containers only block a grant
// when NO slot is held by anyone, because once any slot is held, containers
// are exactly what that holder is expected to be running.
func AcquirePool(townRoot, role string, timeout time.Duration, pool Pool) (*Handle, error) {
	pool = pool.normalized()

	// Reentrant fast path (see reentrantEnvVar): a descendant of a process
	// holding ANY slot of this town does not compete with its ancestor.
	if holderPath, holderPID, ok := reentrantHolder(); ok && holderPID != os.Getpid() {
		if _, isSlot := slotIndexFromLockPath(townRoot, holderPath); isSlot {
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

	grant := func(i int, unlock func()) *Handle {
		h := &Handle{townRoot: townRoot, unlock: unlock, Index: i}
		_ = atomicfile.EnsureDirAndWriteJSON(SlotOwnerPath(townRoot, i), Owner{
			Role:       role,
			PID:        os.Getpid(),
			AcquiredAt: time.Now(),
			Slot:       i,
		})
		_ = os.Setenv(reentrantEnvVar, SlotLockPath(townRoot, i)+"|"+strconv.Itoa(os.Getpid()))
		return h
	}

	for {
		for _, i := range candidates {
			unlock, ok, err := lock.FlockTryAcquire(SlotLockPath(townRoot, i))
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			// We hold slot i. If somebody else holds another slot, any
			// running gate containers are theirs: grant without the
			// docker probe.
			if othersHeld(townRoot, pool, i) > 0 {
				return grant(i, unlock), nil
			}
			containers, containerErr := runningGateContainers()
			switch {
			case containerErr != nil && !isDaemonUnreachable(containerErr):
				// Inconclusive probe (wedged daemon, permission-denied
				// socket): not licensed to assume empty. Release and wait.
				unlock()
			case containerErr != nil:
				fmt.Fprintf(os.Stderr, "gt slot: docker daemon unreachable (%v); proceeding on flock alone\n", containerErr)
				return grant(i, unlock), nil
			case len(containers) == 0:
				return grant(i, unlock), nil
			default:
				// An unwrapped suite has containers up and nobody holds a
				// slot for them. Not safe to hand out ANY slot; release
				// and wait rather than trying the next candidate.
				unlock()
			}
			break
		}
		if hasDeadline && time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for container-gate slot", timeout)
		}
		time.Sleep(DefaultPollInterval)
	}
}

// SlotState is the resolved state of one slot in a StatusPool report.
type SlotState struct {
	Index int    `json:"index"`
	Held  bool   `json:"held"`
	Owner *Owner `json:"owner,omitempty"`
}

// StatusPool is Status over a pool: it reports every slot the pool defines
// plus any extra slot that exists on disk. Report.Held is true when ANY
// slot is held and Report.Owner is the lowest held slot's owner, so
// single-slot callers keep their meaning; Report.Slots carries the full
// picture. The docker cross-check for unwrapped suites runs only when no
// slot is held at all.
func StatusPool(townRoot string, pool Pool) (Report, error) {
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

	if rep.HeldCount == 0 {
		containers, err := runningGateContainers()
		if err != nil {
			rep.DockerUnknown = true
		} else {
			rep.UnwrappedContainers = containers
		}
	}
	return rep, nil
}

func readSlotOwner(townRoot string, i int) *Owner {
	data, err := os.ReadFile(SlotOwnerPath(townRoot, i))
	if err != nil {
		return nil
	}
	var o Owner
	if err := json.Unmarshal(data, &o); err != nil {
		return nil
	}
	o.Slot = i
	return &o
}

// HeldBy returns the slots currently held by role, from a report.
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

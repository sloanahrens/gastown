package slot

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/procid"
)

// The CPU class is a second slot pool beside the container gate: it bounds how
// many CPU-heavy host builds (go build, go vet, go test from a crew checkout)
// run at once, so a burst of crew presubmits cannot starve the daemon, bd and
// the dispatcher. Measured 2026-11-01: nine birds building in parallel took
// host load from 21 to 62 on 24 cores, one `bd list` took 5.9 s and a gt
// install build took over ten minutes (gt-bq68w).
//
// It is deliberately not the container-gate pool: a crew build starts no
// container and must never take a landing slot, and a landing never waits
// behind one. Like the presubmit pool it is a set of kernel-released flocks
// (one per slot), so a holder that dies releases its slot with no reclaim
// path; the owner file beside each lock is display-only metadata.
//
// Waiting callers are served in arrival order. A kernel flock grants whoever
// wins the race, not whoever came first, so the slots are fronted by an
// arrival queue: every caller takes a ticket under a short admission flock and
// only the head of the live queue may take a free slot. A dead waiter's ticket
// is reaped by the same liveness rule the owner files use (Gate.ownerGone).

// DefaultCPUClassSize is how many CPU-class builds may run at once townwide
// when operational.daemon.cpu_class_size is unset.
const DefaultCPUClassSize = config.DefaultCPUClassSize

// DefaultCPUClassWait is how long a CPU-class build waits for a slot when its
// caller names no bound: long enough to sit out a few queued presubmits, short
// enough to fail rather than hang a crew session.
const DefaultCPUClassWait = 30 * time.Minute

// CPULockPath returns the flock file for CPU-class slot i.
func CPULockPath(townRoot string, i int) string {
	return filepath.Join(LockDir(townRoot), fmt.Sprintf("cpu-class-%d.lock", i))
}

// CPUOwnerPath returns the display-only owner file for CPU-class slot i.
func CPUOwnerPath(townRoot string, i int) string {
	return filepath.Join(LockDir(townRoot), fmt.Sprintf("cpu-class-%d.owner", i))
}

// CPUAdmitPath returns the flock that serializes queue updates and slot
// grants. It is held only for the few file reads and writes of one admission,
// never for the slot's lifetime.
func CPUAdmitPath(townRoot string) string {
	return filepath.Join(LockDir(townRoot), "cpu-class-admit.lock")
}

// CPUQueuePath returns the arrival-order queue of waiting callers.
func CPUQueuePath(townRoot string) string {
	return filepath.Join(LockDir(townRoot), "cpu-class.queue")
}

// CPUClassSize is the number of concurrent CPU-class builds townRoot's
// settings/config.json describes (operational.daemon.cpu_class_size).
func CPUClassSize(townRoot string) int {
	return config.LoadOperationalConfig(townRoot).GetDaemonConfig().CPUClassSizeV()
}

// cpuQueue is the arrival queue on disk: the next ticket sequence number and
// every live waiter's ticket, ordered by Seq.
type cpuQueue struct {
	Next    uint64      `json:"next"`
	Tickets []cpuTicket `json:"tickets"`
}

// cpuTicket is one waiter's place in the arrival queue. PID and Start are the
// liveness evidence (the pair names one process for good, see procid.StartToken);
// ID is what this call is known by, so two callers sharing a process — a test
// with two goroutines — hold distinguishable tickets.
type cpuTicket struct {
	ID    string    `json:"id"`
	Seq   uint64    `json:"seq"`
	PID   int       `json:"pid"`
	Start string    `json:"start,omitempty"`
	Role  string    `json:"role,omitempty"`
	At    time.Time `json:"at"`
}

// cpuHolder is one slot's live holder as the timeout and wait messages report
// it. Role and PID are empty when the owner file is missing or names a dead
// process.
type cpuHolder struct {
	Slot int
	Role string
	PID  int
}

// cpuWait describes why one admission pass could not grant. Ahead is how many
// live tickets the caller has in front of it.
type cpuWait struct {
	Holders []cpuHolder
	Ahead   int
}

// describe renders the wait cause for the wait line and the timeout error, so
// both name what the caller is behind rather than just a duration.
func (w cpuWait) describe() string {
	var parts []string
	for _, h := range w.Holders {
		if h.Role != "" {
			parts = append(parts, fmt.Sprintf("%s (pid %d, slot %d)", h.Role, h.PID, h.Slot))
		} else {
			parts = append(parts, fmt.Sprintf("an unnamed holder (slot %d)", h.Slot))
		}
	}
	switch {
	case len(parts) > 0 && w.Ahead > 0:
		return fmt.Sprintf("%d slot(s) held by %s; %d caller(s) queued ahead", len(w.Holders), strings.Join(parts, ", "), w.Ahead)
	case len(parts) > 0:
		return fmt.Sprintf("%d slot(s) held by %s", len(w.Holders), strings.Join(parts, ", "))
	case w.Ahead > 0:
		return fmt.Sprintf("%d caller(s) queued ahead", w.Ahead)
	default:
		return "no slot free"
	}
}

// CPUHandle is a held CPU-class slot. Release exactly once when the build has
// finished; the kernel also releases it if the holder dies.
type CPUHandle struct {
	townRoot string
	// Index is the slot this handle holds.
	Index int
	// WaitedFor is how long the acquire blocked before winning the slot.
	WaitedFor time.Duration
	unlock    func()
	released  bool
}

// Release removes the display-only owner file and drops the flock. It is a
// no-op on a second call, and the kernel would drop the flock on process death
// anyway; this makes a graceful release immediate.
func (h *CPUHandle) Release() error {
	if h.released {
		return nil
	}
	h.released = true
	_ = os.Remove(CPUOwnerPath(h.townRoot, h.Index))
	h.unlock()
	return nil
}

// AcquireCPU blocks until one of size CPU-class slots is free and this caller's
// ticket is at the head of the arrival queue, then holds the slot. timeout <= 0
// waits indefinitely; a caller that exhausts it withdraws its ticket and
// returns an error naming the holders it queued behind. RunCPU is the caller
// (the cpuslot wrapper); the other pools' package-level acquire functions have
// no counterpart here because nothing outside RunCPU takes a CPU-class slot.
func (g *Gate) AcquireCPU(townRoot, role string, timeout time.Duration, size int) (*CPUHandle, error) {
	size = normalizedCPUClassSize(size)
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}
	id, err := newCPUTicketID(g.ticketRand)
	if err != nil {
		return nil, err
	}

	start := g.clock.Now()
	hasDeadline := timeout > 0
	deadline := start.Add(timeout)
	waitLogged := false

	for {
		h, wait, err := g.admitCPU(townRoot, role, id, size)
		if err != nil {
			// This caller will not take a slot this time; its ticket must not
			// park everyone behind it while its process stays alive.
			g.withdrawCPUTicket(townRoot, id)
			return nil, err
		}
		if h != nil {
			h.WaitedFor = g.clock.Since(start)
			return h, nil
		}
		if hasDeadline && g.clock.Now().After(deadline) {
			// A giving-up caller is recorded nowhere else, so it takes its
			// ticket with it: the queue is only waiters, and this one is done
			// waiting.
			g.withdrawCPUTicket(townRoot, id)
			return nil, fmt.Errorf("timed out after %s waiting for a CPU-class build slot (size %d): %s",
				timeout.Round(time.Second), size, wait.describe())
		}
		if !waitLogged {
			waitLogged = true
			capText := ""
			if hasDeadline {
				capText = fmt.Sprintf(" (cap %s)", timeout.Round(time.Second))
			}
			fmt.Fprintf(g.probeOut, "gt slot: waiting for a CPU-class build slot as %s — %s%s\n",
				role, wait.describe(), capText)
		}
		g.clock.Sleep(g.pollInterval)
	}
}

// admitCPU runs one admission pass: join the arrival queue, prune dead tickets
// and, when this caller's ticket is at the head and a slot is free, take it.
// A nil handle with a nil error means the caller must keep waiting.
func (g *Gate) admitCPU(townRoot, role, id string, size int) (*CPUHandle, cpuWait, error) {
	var wait cpuWait
	unlockAdmit, err := lock.FlockAcquire(CPUAdmitPath(townRoot))
	if err != nil {
		return nil, wait, fmt.Errorf("taking the CPU-class admission lock: %w", err)
	}
	defer unlockAdmit()

	state, err := g.readCPUQueue(townRoot)
	if err != nil {
		return nil, wait, err
	}
	pruned := g.pruneCPUTickets(state.Tickets)
	dirty := len(pruned) != len(state.Tickets)
	state.Tickets = pruned
	// The file is rewritten under this lock only, but sorting keeps a
	// hand-edited queue from deciding grants out of arrival order.
	sort.SliceStable(state.Tickets, func(i, j int) bool { return state.Tickets[i].Seq < state.Tickets[j].Seq })

	rank := cpuTicketIndex(state.Tickets, id)
	if rank < 0 {
		state.Tickets = append(state.Tickets, cpuTicket{
			ID:    id,
			Seq:   state.Next,
			PID:   g.pid,
			Start: g.processStart(g.pid),
			Role:  role,
			At:    g.clock.Now(),
		})
		state.Next++
		rank = len(state.Tickets) - 1
		dirty = true
	}
	wait.Ahead = rank

	var free []int
	for i := 0; i < size; i++ {
		unlock, isFree, err := probeCPUSlot(townRoot, i)
		if err != nil {
			return nil, wait, fmt.Errorf("checking CPU-class slot %d: %w", i, err)
		}
		if isFree {
			if unlock != nil {
				unlock()
			}
			free = append(free, i)
			continue
		}
		wait.Holders = append(wait.Holders, g.cpuHolder(townRoot, i))
	}

	// Only the head of the queue may take a slot, so waiters are served in the
	// order they arrived rather than in the order they happen to poll.
	if rank == 0 && len(free) > 0 {
		slot := free[0]
		unlock, ok, err := lock.FlockTryAcquire(CPULockPath(townRoot, slot))
		if err != nil {
			return nil, wait, fmt.Errorf("taking CPU-class slot %d: %w", slot, err)
		}
		if ok {
			writeCPUOwner(townRoot, slot, role, g.pid, g.clock.Now())
			state.Tickets = removeCPUTicket(state.Tickets, id)
			if err := g.writeCPUQueue(townRoot, state); err != nil {
				_ = os.Remove(CPUOwnerPath(townRoot, slot))
				unlock()
				return nil, wait, err
			}
			return &CPUHandle{
				townRoot: townRoot,
				Index:    slot,
				unlock:   pinHold(unlock),
			}, wait, nil
		}
		// The slot was free to the probe but not to this attempt (another
		// reader probing it, or a stale lock file swapped underneath). The
		// ticket stays queued and the next pass tries again.
	}

	if dirty {
		if err := g.writeCPUQueue(townRoot, state); err != nil {
			return nil, wait, err
		}
	}
	return nil, wait, nil
}

// withdrawCPUTicket removes this caller's ticket from the arrival queue. It is
// best-effort: a caller that cannot take the admission lock is leaving a
// ticket its process death will reap.
func (g *Gate) withdrawCPUTicket(townRoot, id string) {
	unlock, err := lock.FlockAcquire(CPUAdmitPath(townRoot))
	if err != nil {
		return
	}
	defer unlock()
	state, err := g.readCPUQueue(townRoot)
	if err != nil {
		return
	}
	kept := removeCPUTicket(state.Tickets, id)
	if len(kept) == len(state.Tickets) {
		return
	}
	state.Tickets = kept
	_ = g.writeCPUQueue(townRoot, state)
}

// pruneCPUTickets drops the tickets of waiters whose process is gone. A ticket
// left by a crashed caller would otherwise hold the head of the queue forever,
// which is the arrival-order counterpart of a stale flock holder.
func (g *Gate) pruneCPUTickets(tickets []cpuTicket) []cpuTicket {
	kept := make([]cpuTicket, 0, len(tickets))
	for _, t := range tickets {
		if t.ID == "" || t.PID <= 0 {
			continue
		}
		if g.ownerGone(&Owner{PID: t.PID, Start: t.Start}) {
			continue
		}
		kept = append(kept, t)
	}
	return kept
}

// probeCPUSlot asks whether CPU-class slot i is held without creating or
// keeping its lock file: free reports that nobody holds it, and unlock releases
// the probe's own momentary hold (nil when there is no lock file to hold).
func probeCPUSlot(townRoot string, i int) (unlock func(), free bool, err error) {
	unlock, err = lock.FlockTryAcquireExisting(CPULockPath(townRoot, i))
	switch {
	case errors.Is(err, lock.ErrFlockHeld):
		return nil, false, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, true, nil
	case err != nil:
		return nil, false, err
	}
	return unlock, true, nil
}

// cpuTicketIndex returns the position of id in tickets, or -1.
func cpuTicketIndex(tickets []cpuTicket, id string) int {
	for i := range tickets {
		if tickets[i].ID == id {
			return i
		}
	}
	return -1
}

// removeCPUTicket returns tickets without id.
func removeCPUTicket(tickets []cpuTicket, id string) []cpuTicket {
	kept := make([]cpuTicket, 0, len(tickets))
	for _, t := range tickets {
		if t.ID != id {
			kept = append(kept, t)
		}
	}
	return kept
}

// cpuHolder reads slot i's owner file for the wait and timeout messages. A
// missing file or a dead owner is reported as an unnamed holder: the flock
// still says the slot is held, and the caller needs to know that either way.
func (g *Gate) cpuHolder(townRoot string, i int) cpuHolder {
	owner := readOwnerFile(CPUOwnerPath(townRoot, i))
	if owner == nil || g.ownerGone(owner) {
		return cpuHolder{Slot: i}
	}
	return cpuHolder{Slot: i, Role: owner.Role, PID: owner.PID}
}

// processStart reads pid's start token through the gate's owner probe, "" when
// the kernel will not say.
func (g *Gate) processStart(pid int) string {
	if g.owner.startToken == nil {
		return ""
	}
	start, _ := g.owner.startToken(pid)
	return start
}

// readCPUQueue reads the arrival queue. A missing file is an empty queue; a
// file that cannot be read or parsed is an error, never an empty queue — an
// unreadable queue would hand out slots out of arrival order with nothing said
// (and the caller gets a message naming the file it must remove).
func (g *Gate) readCPUQueue(townRoot string) (cpuQueue, error) {
	data, err := os.ReadFile(CPUQueuePath(townRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return cpuQueue{}, nil
	}
	if err != nil {
		return cpuQueue{}, fmt.Errorf("reading the CPU-class queue %s: %w", CPUQueuePath(townRoot), err)
	}
	var q cpuQueue
	if err := json.Unmarshal(data, &q); err != nil {
		return cpuQueue{}, fmt.Errorf("parsing the CPU-class queue %s: %w (remove the file to start a fresh queue)", CPUQueuePath(townRoot), err)
	}
	return q, nil
}

// writeCPUQueue replaces the arrival queue atomically. Callers hold the
// admission lock.
func (g *Gate) writeCPUQueue(townRoot string, q cpuQueue) error {
	write := g.writeJSON
	if write == nil {
		write = atomicfile.EnsureDirAndWriteJSON
	}
	if err := write(CPUQueuePath(townRoot), q); err != nil {
		return fmt.Errorf("writing the CPU-class queue: %w", err)
	}
	return nil
}

// writeCPUOwner publishes slot i's holder metadata once its flock is taken. A
// failed write is not fatal: the flock is the truth, and the cost of a missing
// file is a wait message without a name.
func writeCPUOwner(townRoot string, i int, role string, pid int, at time.Time) {
	start, _ := procid.StartToken(pid)
	_ = atomicfile.EnsureDirAndWriteJSON(CPUOwnerPath(townRoot, i), Owner{
		Role:       role,
		PID:        pid,
		Start:      start,
		AcquiredAt: at,
		Slot:       i,
		Settled:    true,
	})
}

// normalizedCPUClassSize applies the pool's invariant: a size below one is the
// default, never zero (a zero pool would park every crew build forever).
func normalizedCPUClassSize(size int) int {
	if size < 1 {
		return DefaultCPUClassSize
	}
	return size
}

// newCPUTicketID returns a random identifier for one acquire call. Randomness,
// not the pid, is what makes two callers in one process distinct; r is the
// gate's entropy source (crypto/rand.Reader unless a test supplies its own).
func newCPUTicketID(r io.Reader) (string, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("generating a CPU-class queue ticket: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// CPUBuildRole reports whether dir is a crew checkout inside townRoot
// (<town>/<rig>/crew/<name>...) and, if so, the role its builds queue under.
// Only crew worktrees run under the CPU class: a polecat's sandboxed pod and
// the landing worker's gate are bounded elsewhere, and their builds must keep
// their current path (gt-bq68w).
func CPUBuildRole(townRoot, dir string) (string, bool) {
	if townRoot == "" || dir == "" {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(townRoot), filepath.Clean(dir))
	if err != nil {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 3 || parts[0] == "." || parts[0] == ".." || parts[1] != "crew" {
		return "", false
	}
	return strings.Join(parts[:3], "/"), true
}

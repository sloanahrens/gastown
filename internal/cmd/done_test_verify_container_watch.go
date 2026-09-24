package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
)

// containerWatchInterval is how often a slot-free gate run polls docker for a
// container it must not run beside, i.e. for an unwrapped container suite.
//
// Five seconds because what the poll has to catch can be short: a
// testcontainers suite's ryuk reaper lives only as long as the test binary
// that started it, so a package running one container-backed test puts a
// matching container up for seconds. One `docker ps` per poll — bounded by
// slot's own dockerPSTimeout when the daemon is wedged — against a suite that
// runs for minutes. A var so tests can drive the watch without waiting out the
// real interval; 0 disables it (see startGateContainerWatch).
var containerWatchInterval = 5 * time.Second

// listGateContainers is the watch's view of the containers running right now.
// A var over slot.GateContainers so no unit test shells out to the real docker
// CLI: a stray dolt/testcontainers/ryuk container on a shared Gas Town host
// would otherwise decide the outcome of a gate test (gt-tuiy).
var listGateContainers = slot.GateContainers

// gateSlotHeld reports whether any container-gate slot is held right now.
// Locks only, deliberately: StatusPool would add a second `docker ps` to every
// poll of a watch that is already polling docker, to re-derive the very
// listing the watch has (gt-a8kx).
//
// A read failure returns held=true with the error. That direction is the
// point: "held" is what stops the watch blaming the run, and a lock state the
// watch cannot read is one it cannot attribute a container through, so the
// honest answer is the one that stays quiet. The error is reported, not
// swallowed, so the log says the watch went blind rather than claiming the
// suite was clean.
var gateSlotHeld = func(townRoot string) (bool, error) {
	rep, err := slot.StatusPoolLocksOnly(townRoot, containerGatePool(townRoot))
	if err != nil {
		return true, err
	}
	return rep.Held || rep.HeldCount > 0, nil
}

// containerWatch watches a gate run that holds no container-gate slot for a
// container appearing beside it (gt-0ss4).
//
// gt-wx53 decides whether the gate must hold the town-wide container-gate slot
// from the rig's COMMAND TEXT: a Go rig whose command carries no literal
// GT_TEST_DOCKER=1 runs the suite with the opt-in written off and takes no
// slot. That text is a proxy for the thing the rule is actually about — a
// container starting — and it has a hole: a Makefile target that exports the
// opt-in itself, a test that starts a container unconditionally, or a wrapper
// script all start one without the token in that text (gt-0ss4).
//
// This is the observable that closes it. A slot-free run cannot hold a
// container legitimately: the Docker VM is shared town-wide and only a slot
// holder may use it. So a container that was not running when the run started
// and that no slot holder owns is one this run must not run beside — usually
// the run's own, started by a suite that ignored the opt-out the gate wrote,
// but the gate cannot always tell that from someone else's unwrapped suite,
// and `gt slot status` reports both as the same state. The gate fails on it
// either way rather than adding a suite to a contended VM.
//
// Failing rather than acquiring is deliberate. Acquiring after the fact would
// not restore the exclusion the slot exists for — the container is already
// competing for the VM — and it would hide a rig bug worth surfacing: a rig
// whose command starts containers must declare the opt-in so the gate takes a
// slot BEFORE the run, which is one line of config and a green gate.
type containerWatch struct {
	interval   time.Duration
	containers func() ([]slot.GateContainer, error)
	slotHeld   func(townRoot string) (bool, error)
	townRoot   string
	logFile    *os.File
	// cancelRun kills the run the watch is watching, once it has a container
	// to blame. The watch is the only caller.
	cancelRun context.CancelFunc

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}

	mu sync.Mutex
	// baseline is every gate container running when the run started, by
	// display name. A container in here was not started by this run, so the
	// watch never blames it — which is what keeps pre-existing debris
	// (gt-ul1k) from failing an innocent gate.
	baseline map[string]bool
	// pending is the new containers the previous poll saw and this one has not
	// confirmed. A container has to survive two polls before it is blamed:
	// the holder that started it releases the token as its suite ends, a beat
	// before testcontainers' ryuk reaps its containers, and a single poll can
	// land in that gap (see poll).
	pending map[string]bool
	// stray is the container(s) blamed for this run, empty until then.
	stray []string
	// notedOthers is the containers already reported as another holder's, so
	// a suite running beside this run is reported once rather than every poll.
	notedOthers map[string]bool
	// blind is every reason the watch could not see or could not attribute,
	// in the order it happened. Reported in the gate log: a watch that could
	// not have caught anything must not read as one that looked and found
	// nothing.
	blind []string

	polls   int
	started time.Time
}

// startGateContainerWatch begins watching a slot-free gate run. runCtx bounds
// the watch's life (the watch stops with the run it is watching); cancelRun
// kills the run when the watch has a container to blame. Returns nil when the
// watch cannot be established, having written why to the log — a watch that
// cannot establish a baseline cannot tell this run's containers from anyone
// else's and must not fail the run on a container that was already there. The
// town's own detector for that state is `gt slot status`, which reports any
// container running without a token as an unwrapped suite.
func startGateContainerWatch(runCtx context.Context, cancelRun context.CancelFunc, townRoot string, logFile *os.File) *containerWatch {
	if containerWatchInterval <= 0 {
		return nil
	}
	w := &containerWatch{
		interval:   containerWatchInterval,
		containers: listGateContainers,
		slotHeld:   gateSlotHeld,
		townRoot:   townRoot,
		logFile:    logFile,
		cancelRun:  cancelRun,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		started:    time.Now(),
	}
	baseline, err := w.containers()
	if err != nil {
		reportVerifyProgress(logFile, fmt.Sprintf(
			"container watch: cannot list running containers (%v) — this run's container cannot be told from one that was already there, so the watch will not fail this run; `gt slot status` reports unwrapped containers for the town", err))
		return nil
	}
	w.baseline = containerNameSet(containerDisplays(baseline))
	reportVerifyProgress(logFile, fmt.Sprintf(
		"container watch: no slot is held for this run, so a container that appears and no slot holder owns is this run's — polling every %s (gt-0ss4)", w.interval))
	go w.loop(runCtx)
	return w
}

// loop polls until the run it watches ends or the watch is stopped.
func (w *containerWatch) loop(runCtx context.Context) {
	defer close(w.doneCh)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-runCtx.Done():
			return
		case <-ticker.C:
			w.poll()
		}
	}
}

// poll takes one look at the host and decides what it means for this run.
func (w *containerWatch) poll() {
	w.mu.Lock()
	w.polls++
	w.mu.Unlock()

	current, err := w.containers()
	if err != nil {
		w.noteBlind(fmt.Sprintf("listing running containers: %v", err))
		return
	}
	fresh := w.untracked(containerDisplays(current))
	if len(fresh) == 0 {
		w.setPending(nil)
		return
	}

	// A live holder is the one legitimate owner of containers, so its
	// containers are not this run's — including one it starts now, because
	// the token is taken before the suite that starts it.
	held, heldErr := w.slotHeld(w.townRoot)
	if heldErr != nil {
		w.noteBlind(fmt.Sprintf("reading the container-gate slot: %v", heldErr))
		w.setPending(nil)
		return
	}
	if held {
		w.setPending(nil)
		w.noteOthers(fresh)
		return
	}

	// Two consecutive polls, not one. The holder above releases the token as
	// its suite returns, a beat before testcontainers' ryuk reaps the
	// containers that suite left behind; a poll landing in that beat sees a
	// live holder's containers with no holder. Requiring the container to
	// still be there a poll later costs the gate a few seconds of detection
	// latency it does not care about and cannot blame a container on its way
	// out. It also drops a one-off `docker run` by a bystander, which is not
	// something a gate run started.
	confirmed := intersectNamed(w.pendingNow(), fresh)
	if len(confirmed) == 0 {
		w.setPending(fresh)
		return
	}
	w.blame(confirmed)
}

// blame records the containers this run started and kills the run.
func (w *containerWatch) blame(containers []string) {
	w.mu.Lock()
	first := len(w.stray) == 0
	if first {
		w.stray = append([]string(nil), containers...)
		w.pending = nil
	}
	w.mu.Unlock()
	if !first {
		return
	}
	reportVerifyProgress(w.logFile, fmt.Sprintf(
		"container watch: %s appeared during this slot-free run and no container-gate slot is held — nothing owns it, and this run must not share the Docker VM with an unwrapped suite; failing the gate (gt-0ss4)", strings.Join(containers, ", ")))
	w.cancelRun()
}

// stop ends the watch and waits for the polling goroutine to leave, so the
// summary below cannot race a poll still in flight. The wait is bounded by the
// container listing's own timeout.
func (w *containerWatch) stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.doneCh
}

// strayContainers returns the containers this run is blamed for starting, or
// nil. Read after stop.
func (w *containerWatch) strayContainers() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.stray...)
}

// summary is the watch's evidence line for a gate that finished without a
// container to blame: what it looked at, how often, and every reason it could
// not see. The slot-free decision is a proxy for "this suite starts no
// container", and this line is the observation that says the proxy held for
// this run — or says why it was not checked.
func (w *containerWatch) summary() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := fmt.Sprintf("container watch: no container appeared in %d poll(s) over %s",
		w.polls, time.Since(w.started).Round(time.Second))
	if len(w.blind) > 0 {
		s += "; incomplete: " + strings.Join(w.blind, "; ")
	}
	return s
}

// untracked returns the displays in current that neither the baseline nor the
// report already covers.
func (w *containerWatch) untracked(current []string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var fresh []string
	for _, d := range current {
		if w.baseline[d] || w.notedOthers[d] {
			continue
		}
		fresh = append(fresh, d)
	}
	sort.Strings(fresh)
	return fresh
}

func (w *containerWatch) pendingNow() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending
}

func (w *containerWatch) setPending(next []string) {
	w.mu.Lock()
	w.pending = containerNameSet(next)
	w.mu.Unlock()
}

// nameSet is a display set, for the membership tests a poll makes.
func containerNameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// noteOthers reports containers a live holder owns, once per container.
func (w *containerWatch) noteOthers(containers []string) {
	w.mu.Lock()
	var newly []string
	if w.notedOthers == nil {
		w.notedOthers = map[string]bool{}
	}
	for _, d := range containers {
		if !w.notedOthers[d] {
			w.notedOthers[d] = true
			newly = append(newly, d)
		}
	}
	w.mu.Unlock()
	if len(newly) == 0 {
		return
	}
	reportVerifyProgress(w.logFile, fmt.Sprintf(
		"container watch: %s appeared while another owner holds the container-gate slot — theirs, not this run's", strings.Join(newly, ", ")))
}

// noteBlind records a poll that could not see or could not attribute, once per
// distinct reason.
func (w *containerWatch) noteBlind(reason string) {
	w.mu.Lock()
	for _, seen := range w.blind {
		if seen == reason {
			w.mu.Unlock()
			return
		}
	}
	w.blind = append(w.blind, reason)
	w.mu.Unlock()
	reportVerifyProgress(w.logFile, fmt.Sprintf(
		"container watch: %s — this poll could not attribute containers (gt-0ss4)", reason))
}

// displays is the watch's identity for a container: what `gt slot status` and
// the reap warnings call it, and unique per container in docker's own listing
// (testcontainers names every container and every ryuk reaper for its session).
func containerDisplays(containers []slot.GateContainer) []string {
	out := make([]string, 0, len(containers))
	for _, c := range containers {
		out = append(out, c.Display())
	}
	sort.Strings(out)
	return out
}

// intersectNamed returns the members of set that appear in names, sorted and
// deduplicated.
func intersectNamed(set map[string]bool, names []string) []string {
	var both []string
	seen := map[string]bool{}
	for _, n := range names {
		if set[n] && !seen[n] {
			seen[n] = true
			both = append(both, n)
		}
	}
	sort.Strings(both)
	return both
}

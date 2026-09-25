package slot

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Owner labels bind a test container to the process that started it
// (gt-ehlga). The gate's other evidence — age, a ryuk reaper for the session,
// image, a before/after diff — is a proxy for ownership, and each proxy can be
// wrong in the direction that either leaks a container or kills a live run's.
// A failed or killed gate left three Dolt containers with no ryuk reaper and
// no live test process, and the next gate waited on them for 10m28s because
// they were younger than the 30m staleness window.
//
// internal/testutil stamps these labels on every Dolt container it starts,
// naming the test binary's own pid, host and start time. Classify then reads a
// container whose owner is certainly gone — no such pid, or the pid now names a
// process with a different start time — as debris at any age, and one whose
// owner is confirmed alive (same pid, same start time) as a live suite at any
// age. When the owner only looks alive (the start time cannot be compared) the
// labels decide only while the container is younger than the staleness
// window; past it the age/reaper rules decide, so a dead owner's reused pid
// cannot hold the gate forever. A container without the labels, or labeled on
// another host, is judged by the older evidence exactly as before.
const (
	// OwnerPIDLabel carries the pid of the test process that started the
	// container.
	OwnerPIDLabel = "gastown.test.owner-pid"
	// OwnerHostLabel carries that process's hostname: a pid only means
	// something in the pid namespace it came from, so a container labeled on
	// another host (a remote docker, a devcontainer sharing the socket) is not
	// judged by this host's process table.
	OwnerHostLabel = "gastown.test.owner-host"
	// OwnerStartLabel carries the owner's process start time as the kernel
	// reports it (processStartToken). A pid is reused once its process dies;
	// a start time is not, so the pair names one process for good.
	OwnerStartLabel = "gastown.test.owner-start"
)

// TestContainerOwnerLabels returns the owner labels for a container the
// calling process is about to start. It returns nil when the hostname cannot
// be read: an unlabeled container falls back to the gate's older evidence,
// which is safe, while a label that names no host could not be judged.
func TestContainerOwnerLabels() map[string]string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return nil
	}
	labels := map[string]string{
		OwnerPIDLabel:  strconv.Itoa(os.Getpid()),
		OwnerHostLabel: host,
	}
	// Without a start time the owner can still be judged, only less strongly:
	// see ownerVerdict.
	if start, ok := ownerStartToken(os.Getpid()); ok {
		labels[OwnerStartLabel] = start
	}
	return labels
}

// OwnerProcess returns the pid and host the container's owner labels name; ok is
// false when either is missing or the pid is not a positive integer.
func (c GateContainer) OwnerProcess() (pid int, host string, ok bool) {
	host = c.Labels[OwnerHostLabel]
	pid, err := strconv.Atoi(c.Labels[OwnerPIDLabel])
	if err != nil || pid <= 0 || host == "" {
		return 0, "", false
	}
	return pid, host, true
}

// localHostname is os.Hostname behind a var so a test can pin the host the
// owner labels are compared against.
var localHostname = os.Hostname

// ownerProcessGone reports whether no process with this pid exists on this
// host. It must answer true only when that is certain: a true answer licenses
// deleting the pid's containers. A process owned by another user (EPERM), any
// other probe error, and a platform with no such probe all answer false —
// the container is then judged by the older evidence instead. A reused pid
// also answers false here; ownerStartToken is what tells a reused pid apart.
// Declared as a var so tests never probe the real process table.
var ownerProcessGone = processGone

// ownerStartToken reads a live pid's start time (processStartToken); ok is
// false when it cannot be read. A var so tests can stage a reused pid.
var ownerStartToken = processStartToken

// SetOwnerProcessProbeForTest overrides ownerProcessGone and localHostname for
// tests in other packages that drive Acquire, Status or Reap over fake
// labeled containers. Returns a restore func the caller must invoke.
//
// The start-time probe is set to "unreadable", so owner verdicts use the
// weaker, age-bounded reading unless a test stages start times itself.
func SetOwnerProcessProbeForTest(gone func(pid int) bool, hostname string) (restore func()) {
	prevGone, prevHost, prevStart := ownerProcessGone, localHostname, ownerStartToken
	ownerProcessGone = gone
	localHostname = func() (string, error) { return hostname, nil }
	ownerStartToken = func(int) (string, bool) { return "", false }
	return func() { ownerProcessGone, localHostname, ownerStartToken = prevGone, prevHost, prevStart }
}

// ownerVerdict judges a container by its owner labels. ok is false when the
// labels cannot decide — absent, malformed, naming another host, or an owner
// that only looks alive on a container past the staleness window — and the
// caller falls back to the age/reaper rules.
//
// "Gone" is certain in two ways: the pid does not exist (ESRCH), or it exists
// with a start time other than the one recorded at container creation — the
// original process died and its pid was reused. "Alive" is certain only when
// the start times match; then the container is live at any age, because its
// owner is provably the process that started it. When either start time is
// unavailable the pid alone cannot rule out reuse, so it counts as alive only
// while the container is younger than window: past that, the age/ryuk rules
// decide as they did before owner labels existed, and a reused pid can never
// hold the gate for longer than it could before (gt-ehlga review).
func ownerVerdict(c GateContainer, now time.Time, window time.Duration) (verdict ContainerVerdict, ok bool) {
	pid, host, labeled := c.OwnerProcess()
	if !labeled {
		return ContainerVerdict{}, false
	}
	local, err := localHostname()
	if err != nil || local != host {
		return ContainerVerdict{}, false
	}
	gone := func(reason string) (ContainerVerdict, bool) {
		return ContainerVerdict{Container: c, Verdict: VerdictDebris, Reason: reason, OwnerGone: true}, true
	}
	if ownerProcessGone(pid) {
		return gone("its owning test process (pid " + strconv.Itoa(pid) + " on " + host + ") is gone")
	}
	recorded := c.Labels[OwnerStartLabel]
	current, readable := ownerStartToken(pid)
	if recorded != "" && readable {
		if current != recorded {
			return gone(fmt.Sprintf("its owning test process (pid %d on %s, started %s) is gone; pid %d now names a process started %s",
				pid, host, recorded, pid, current))
		}
		return ContainerVerdict{
			Container: c,
			Verdict:   VerdictLive,
			Reason:    fmt.Sprintf("its owning test process (pid %d, started %s) is still running", pid, recorded),
		}, true
	}
	// Start time unverifiable: the pid may have been reused.
	if age, known := c.Age(now); known && age >= window {
		return ContainerVerdict{}, false
	}
	return ContainerVerdict{
		Container: c,
		Verdict:   VerdictLive,
		Reason:    "its owning test process (pid " + strconv.Itoa(pid) + ") is still running (start time unverified)",
	}, true
}

// orphanRemover returns a func that force-removes one owner-gone container,
// once per container, logging what it removed and why to debrisWriter. Acquire
// calls it for the verdicts Classify marked OwnerGone: the owner process is
// certainly gone, so nothing live can be using the container, and leaving it
// up would keep an ownerless container on the shared Docker VM (gt-ehlga).
// Anything else — an unlabeled or age-only debris container — is never
// removed here.
func orphanRemover() func(ContainerVerdict) {
	tried := map[string]bool{}
	return func(verdict ContainerVerdict) {
		if !verdict.OwnerGone || verdict.Container.ID == "" || tried[verdict.Container.ID] {
			return
		}
		tried[verdict.Container.ID] = true
		if err := removeContainer(verdict.Container.ID); err != nil {
			fmt.Fprintf(debrisWriter, "gt slot: could not remove orphaned %s (%s): %v. 'gt slot reap' will retry.\n",
				verdict.Container.Display(), verdict.Reason, err)
			return
		}
		fmt.Fprintf(debrisWriter, "gt slot: removed orphaned %s (id %s): %s; labels: %s\n",
			verdict.Container.Display(), verdict.Container.ID, verdict.Reason, verdict.Container.LabelSummary())
	}
}

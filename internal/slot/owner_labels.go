package slot

import (
	"fmt"
	"os"
	"strconv"
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
// naming the test binary's own pid and host. Classify then reads a container
// whose owner is certainly gone as debris at any age, and one whose owner is
// alive as a live suite at any age; a container without the labels, or
// labeled on another host, is judged by the older evidence exactly as before.
const (
	// OwnerPIDLabel carries the pid of the test process that started the
	// container.
	OwnerPIDLabel = "gastown.test.owner-pid"
	// OwnerHostLabel carries that process's hostname: a pid only means
	// something in the pid namespace it came from, so a container labeled on
	// another host (a remote docker, a devcontainer sharing the socket) is not
	// judged by this host's process table.
	OwnerHostLabel = "gastown.test.owner-host"
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
	return map[string]string{
		OwnerPIDLabel:  strconv.Itoa(os.Getpid()),
		OwnerHostLabel: host,
	}
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
// also answers false, which keeps a container rather than removing one.
// Declared as a var so tests never probe the real process table.
var ownerProcessGone = processGone

// SetOwnerProcessProbeForTest overrides ownerProcessGone and localHostname for
// tests in other packages that drive Acquire, Status or Reap over fake
// labeled containers. Returns a restore func the caller must invoke.
func SetOwnerProcessProbeForTest(gone func(pid int) bool, hostname string) (restore func()) {
	prevGone, prevHost := ownerProcessGone, localHostname
	ownerProcessGone = gone
	localHostname = func() (string, error) { return hostname, nil }
	return func() { ownerProcessGone, localHostname = prevGone, prevHost }
}

// ownerVerdict judges a container by its owner labels. ok is false when the
// labels cannot decide — absent, malformed, or naming another host — and the
// caller falls back to the age/reaper rules.
func ownerVerdict(c GateContainer) (verdict ContainerVerdict, ok bool) {
	pid, host, labeled := c.OwnerProcess()
	if !labeled {
		return ContainerVerdict{}, false
	}
	local, err := localHostname()
	if err != nil || local != host {
		return ContainerVerdict{}, false
	}
	if ownerProcessGone(pid) {
		return ContainerVerdict{
			Container: c,
			Verdict:   VerdictDebris,
			Reason:    "its owning test process (pid " + strconv.Itoa(pid) + " on " + host + ") is gone",
			OwnerGone: true,
		}, true
	}
	return ContainerVerdict{
		Container: c,
		Verdict:   VerdictLive,
		Reason:    "its owning test process (pid " + strconv.Itoa(pid) + ") is still running",
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

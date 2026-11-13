package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// readyClock is the clock a ready wait reads and advances. Each pause the
// backend takes moves now forward by the pause, so a test reads how long the
// wait ran without sleeping.
type readyClock struct {
	now      time.Time
	waits    int
	warnings []string
}

// setReadyClock puts b's ready wait on c, and routes its warnings into c.
func setReadyClock(b *openshellBackend, c *readyClock) {
	b.now = func() time.Time { return c.now }
	b.wait = func(_ context.Context, d time.Duration) error {
		c.waits++
		c.now = c.now.Add(d)
		return nil
	}
	b.warnf = func(format string, args ...any) {
		c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
	}
}

// podScheduledReply is a sandbox still Provisioning whose PodScheduled condition
// reads status, with reason and the given transition time. An empty reason and
// message leave them out, as a condition that carries none would.
func podScheduledReply(status, reason, message, lastTransition string) string {
	return fmt.Sprintf(`{"name":%q,"phase":"Provisioning","conditions":[{"type":"Ready","status":"False"},{"type":"PodScheduled","status":%q,"reason":%q,"message":%q,"lastTransitionTime":%q}]}`,
		openshellTestSession, status, reason, message, lastTransition)
}

// unschedulableReply is the scheduler's answer for a pod it cannot place.
func unschedulableReply(lastTransition string) string {
	return podScheduledReply("False", "Unschedulable", "0/3 nodes are available: Insufficient memory.", lastTransition)
}

func TestOpenShellPrepareGivesUpOnAnUnschedulablePodAtTheBound(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	base := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	fake := openshellPrepareFake(map[string]string{"get": unschedulableReply(base.Format(time.RFC3339))})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	clock := &readyClock{now: base}
	setReadyClock(b, clock)

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	var unsched *sandboxUnschedulableError
	if !errors.As(err, &unsched) {
		t.Fatalf("err = %v, want the unschedulable placement failure", err)
	}
	if unsched.Sandbox != openshellTestSession {
		t.Errorf("failed placement names sandbox %q, want %q", unsched.Sandbox, openshellTestSession)
	}
	if !strings.Contains(err.Error(), "Insufficient memory") {
		t.Errorf("err = %v, want the scheduler's message in it", err)
	}
	if !errors.Is(err, ErrSandboxUnplaceable) {
		t.Errorf("err = %v, want it to match ErrSandboxUnplaceable so the dispatcher defers the bead", err)
	}
	if got := clock.now.Sub(base); got != unschedulableGiveUp {
		t.Errorf("the wait ended %s after the condition turned, want %s", got, unschedulableGiveUp)
	}
	if unsched.Waited != unschedulableGiveUp {
		t.Errorf("Waited = %s, want %s", unsched.Waited, unschedulableGiveUp)
	}
}

func TestOpenShellPrepareUnschedulableRollsBackThePodAndTheSeat(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	base := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	fake := openshellPrepareFake(map[string]string{"get": unschedulableReply(base.Format(time.RFC3339))})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	setReadyClock(b, &readyClock{now: base})
	writeTrackerRoute(t, town, openshellTestRig)
	trackerSeatToken(t, b, town)
	revoker := &revokeFake{}
	b.revoker = revoker

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	var unsched *sandboxUnschedulableError
	if !errors.As(err, &unsched) {
		t.Fatalf("err = %v, want the unschedulable placement failure", err)
	}
	last := fake.Calls()[len(fake.Calls())-1]
	if want := []string{"openshell", "sandbox", "delete", openshellTestSession}; !reflect.DeepEqual(last, want) {
		t.Fatalf("last call = %q, want %q (the pod deleted by the rollback)", last, want)
	}
	if want := wantActors(openshellTestRig, openshellTestPolecat); !slices.Equal(revoker.actors, want) {
		t.Errorf("revoked %v, want %v: the seat is released with the pod", revoker.actors, want)
	}
	if _, ok, _ := ReadSandboxRecord(town, openshellTestSession); ok {
		t.Fatal("the rollback left the sandbox record behind")
	}
}

func TestOpenShellPrepareUnschedulableGateKeepsWaitingForAScheduledPodPastTheBound(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	base := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	fake := openshellPrepareFake(map[string]string{"get": podScheduledReply("True", "", "", base.Format(time.RFC3339))})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	clock := &readyClock{now: base}
	setReadyClock(b, clock)

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	var unsched *sandboxUnschedulableError
	if errors.As(err, &unsched) {
		t.Fatalf("err = %v, a scheduled pod must not be given up on", err)
	}
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("not Ready after %d polls", readyPolls)) {
		t.Fatalf("err = %v, want the readiness timeout", err)
	}
	if clock.now.Sub(base) <= unschedulableGiveUp {
		t.Errorf("the wait ran %s, want it past %s", clock.now.Sub(base), unschedulableGiveUp)
	}
	if clock.waits != readyPolls-1 {
		t.Errorf("waits = %d, want %d", clock.waits, readyPolls-1)
	}
}

func TestOpenShellPrepareUnreadableUnschedulableTimeKeepsTheWaitAndWarnsOnce(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	base := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	fake := openshellPrepareFake(map[string]string{"get": unschedulableReply("yesterday")})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	clock := &readyClock{now: base}
	setReadyClock(b, clock)

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	var unsched *sandboxUnschedulableError
	if errors.As(err, &unsched) {
		t.Fatalf("err = %v, an unreadable time must never shorten the wait", err)
	}
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("not Ready after %d polls", readyPolls)) {
		t.Fatalf("err = %v, want the readiness timeout", err)
	}
	if clock.waits != readyPolls-1 {
		t.Errorf("waits = %d, want %d", clock.waits, readyPolls-1)
	}
	var logged []string
	for _, w := range clock.warnings {
		if strings.Contains(w, "PodScheduled") {
			logged = append(logged, w)
		}
	}
	if len(logged) != 1 {
		t.Errorf("PodScheduled warnings = %q, want exactly one for the wait", logged)
	}
}

// unschedulableWaitPauses is how many pauses a give-up at unschedulableGiveUp
// takes when the clock starts at the first poll: the first poll is at zero, and
// each pause adds readyInterval.
const unschedulableWaitPauses = int(unschedulableGiveUp / readyInterval)

func TestOpenShellPrepareUnschedulableGivesUpAtTheBoundOnTheHostClock(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	base := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	// The gateway's transition time is an hour behind this host's clock. Taking
	// the bound from that timestamp would give up on the first poll; the clock
	// must start at the first poll instead.
	fake := openshellPrepareFake(map[string]string{"get": unschedulableReply(base.Add(-time.Hour).Format(time.RFC3339))})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	clock := &readyClock{now: base}
	setReadyClock(b, clock)

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	var unsched *sandboxUnschedulableError
	if !errors.As(err, &unsched) {
		t.Fatalf("err = %v, want the unschedulable placement failure", err)
	}
	if clock.waits != unschedulableWaitPauses {
		t.Errorf("waits = %d, want %d: the give-up comes %s after the first poll", clock.waits, unschedulableWaitPauses, unschedulableGiveUp)
	}
	if got := clock.now.Sub(base); got != unschedulableGiveUp {
		t.Errorf("the wait ended %s after the first poll, want %s", got, unschedulableGiveUp)
	}
}

func TestOpenShellPrepareUnschedulableNeverGivesUpOnAClockThatReadsBackwards(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	base := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	// The gateway's transition time is an hour ahead of this host's clock. The
	// wait must not run for that hour on the first poll: the bound is the same
	// 90 host-clock seconds from the first poll as with any other timestamp.
	fake := openshellPrepareFake(map[string]string{"get": unschedulableReply(base.Add(time.Hour).Format(time.RFC3339))})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	clock := &readyClock{now: base}
	setReadyClock(b, clock)

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	var unsched *sandboxUnschedulableError
	if !errors.As(err, &unsched) {
		t.Fatalf("err = %v, want the unschedulable placement failure", err)
	}
	if clock.waits != unschedulableWaitPauses {
		t.Errorf("waits = %d, want %d", clock.waits, unschedulableWaitPauses)
	}
	if got := clock.now.Sub(base); got != unschedulableGiveUp {
		t.Errorf("the wait ended %s after the first poll, want %s", got, unschedulableGiveUp)
	}
}

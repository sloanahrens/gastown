package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/config"
)

// driftRun is a reconcile started with a gateway named, whose pusher stores the
// tree "sha". at holds the clock's time at each push, in order, so a test can
// read when each push happened as well as that it happened.
type driftRun struct {
	d      *Daemon
	clk    *driftSpyClock
	pusher *fakeWorkspacePusher
	at     []time.Time
}

// startDriftRun starts the reconcile and waits for its start push, which stores
// "sha" at testEpoch. The push is received before the loop records the push time
// and sets the tick, so the wait for the tick comes before any clock move.
func startDriftRun(t *testing.T) *driftRun {
	t.Helper()
	r := &driftRun{clk: newDriftSpyClock()}
	r.pusher = &fakeWorkspacePusher{calls: make(chan int, 10)}
	r.pusher.onCall = func(int) { r.at = append(r.at, r.clk.Now()) }
	r.d = newWorkspaceDaemon(t, gatewayEnv(), r.clk, r.pusher, &lockedBuffer{}, nil)
	if !r.d.startWorkspaceReconcile() {
		t.Fatal("startWorkspaceReconcile() = false with a gateway named, want true")
	}
	if got := awaitPush(t, r.pusher.calls); got != 1 {
		t.Fatalf("start push = call %d, want 1", got)
	}
	awaitWaits(t, r.clk.FakeClock, 1)
	return r
}

// awaitWaits waits until the reconcile holds n clock waits, so a clock move after
// it reaches the timers an answer has set.
func awaitWaits(t *testing.T, clk *clockwork.FakeClock, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), workspaceHangGuard)
	defer cancel()
	if err := clk.BlockUntilContext(ctx, n); err != nil {
		t.Fatalf("the reconcile holds fewer than %d clock waits: %v", n, err)
	}
}

// TestWorkspaceDriftAnswers: which answers drift. A hash that matches the last
// good push, or an answer with no header, triggers nothing. A hash that differs,
// or a refusal that the workspace is not present, triggers.
func TestWorkspaceDriftAnswers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		answer bdgateway.WorkspaceAnswer
		want   bool
	}{
		{name: "a matching hash triggers nothing", answer: bdgateway.WorkspaceAnswer{Hash: "sha"}, want: false},
		{name: "an answer with no header triggers nothing", answer: bdgateway.WorkspaceAnswer{}, want: false},
		{name: "a differing hash triggers", answer: bdgateway.WorkspaceAnswer{Hash: "other"}, want: true},
		{name: "a refusal that the workspace is not present triggers", answer: bdgateway.WorkspaceAnswer{Absent: true}, want: true},
		{name: "a refusal that the workspace is not present triggers with the matching hash", answer: bdgateway.WorkspaceAnswer{Hash: "sha", Absent: true}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := workspaceDrifted(tt.answer, "sha"); got != tt.want {
				t.Errorf("workspaceDrifted(%+v, \"sha\") = %v, want %v", tt.answer, got, tt.want)
			}
		})
	}
}

// TestWorkspaceDriftPushesBeforeNextTick: a drift answer pushes before the next
// interval. A push a minute after the last is at once; one within the gap waits
// for the gap to end. Both come before the five-minute tick.
func TestWorkspaceDriftPushesBeforeNextTick(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		answer    bdgateway.WorkspaceAnswer
		sinceLast time.Duration
		wantAt    time.Duration
	}{
		{
			name:      "a differing hash a minute after the last push pushes at once",
			answer:    bdgateway.WorkspaceAnswer{Hash: "other"},
			sinceLast: time.Minute,
			wantAt:    time.Minute,
		},
		{
			name:      "a differing hash within the gap pushes when the gap ends",
			answer:    bdgateway.WorkspaceAnswer{Hash: "other"},
			sinceLast: 0,
			wantAt:    workspaceDriftGap,
		},
		{
			name:      "a refusal that the workspace is not present a minute after the last push pushes at once",
			answer:    bdgateway.WorkspaceAnswer{Absent: true},
			sinceLast: time.Minute,
			wantAt:    time.Minute,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := startDriftRun(t)
			r.clk.Advance(tt.sinceLast)
			r.d.workspaceDrift.observe(tt.answer)
			if tt.wantAt > tt.sinceLast {
				awaitWaits(t, r.clk.FakeClock, 2)
				r.clk.Advance(tt.wantAt - tt.sinceLast)
			}
			if got := awaitPush(t, r.pusher.calls); got != 2 {
				t.Fatalf("push = call %d, want the drift push 2", got)
			}
			got := r.at[1].Sub(r.at[0])
			if got != tt.wantAt {
				t.Errorf("drift push at %s after the last push, want %s", got, tt.wantAt)
			}
			if got >= config.DefaultWorkspaceReconcileInterval {
				t.Errorf("drift push at %s, not before the next interval %s", got, config.DefaultWorkspaceReconcileInterval)
			}
		})
	}
}

// TestWorkspaceDriftCoalesces: two drift answers inside one gap make one push. The
// push after the gap resets the interval, so the next push is the tick a full
// interval later. So push 3 must be that tick: an extra drift push would land at
// the gap and fail the time check.
func TestWorkspaceDriftCoalesces(t *testing.T) {
	t.Parallel()
	r := startDriftRun(t)
	r.d.workspaceDrift.observe(bdgateway.WorkspaceAnswer{Hash: "other"})
	r.d.workspaceDrift.observe(bdgateway.WorkspaceAnswer{Absent: true})
	awaitWaits(t, r.clk.FakeClock, 2)
	r.clk.Advance(workspaceDriftGap)
	if got := awaitPush(t, r.pusher.calls); got != 2 {
		t.Fatalf("push = call %d, want the one drift push 2", got)
	}
	// awaitWaits cannot stand here: the old tick and the tick push 2 re-arms are
	// each one wait, so it cannot see that pushNow has stopped the old tick and
	// armed the new one. A clock move that lands first fires the old tick, which
	// the loop no longer reads, and the new tick is armed at the moved time, so
	// push 3 never comes. The count reaches three only once pushNow has re-armed
	// the tick: the start tick, the deferred wait, and the new tick.
	awaitDriftTimers(t, r.clk, 3)
	r.clk.Advance(config.DefaultWorkspaceReconcileInterval)
	if got := awaitPush(t, r.pusher.calls); got != 3 {
		t.Fatalf("push = call %d, want the tick push 3", got)
	}
	if got, want := r.at[2].Sub(r.at[0]), workspaceDriftGap+config.DefaultWorkspaceReconcileInterval; got != want {
		t.Errorf("push 3 at %s after the start, want %s: the two answers made more than one drift push", got, want)
	}
}

// TestWorkspaceDriftMatchingHashIsQuiet: a hash that matches the last good push
// records no drift. The check runs without a loop, because a running loop takes a
// drift as soon as it is recorded and the record could not be read back. The
// second observation is a control: a differing hash must record one, or the first
// proves nothing.
func TestWorkspaceDriftMatchingHashIsQuiet(t *testing.T) {
	t.Parallel()
	c := newWorkspaceDriftCheck()
	c.stored("sha")
	c.observe(bdgateway.WorkspaceAnswer{Hash: "sha"})
	if pending, _ := c.take(); pending {
		t.Fatal("a matching hash recorded a drift, want none")
	}
	c.observe(bdgateway.WorkspaceAnswer{Hash: "other"})
	if pending, _ := c.take(); !pending {
		t.Fatal("a differing hash recorded no drift, want one")
	}
}

// TestWorkspaceDriftOwnAnswerIsQuiet: an answer to the loop's own push is never
// drift, whatever its hash, because that push wrote the tree the answer echoes. The
// foreign answer after it is the control: it must record a drift.
func TestWorkspaceDriftOwnAnswerIsQuiet(t *testing.T) {
	t.Parallel()
	c := newWorkspaceDriftCheck()
	c.stored("old")
	c.observe(bdgateway.WorkspaceAnswer{Hash: "new", Own: true})
	c.observe(bdgateway.WorkspaceAnswer{Absent: true, Own: true})
	if pending, _ := c.take(); pending {
		t.Fatal("an answer to the loop's own push recorded a drift, want none")
	}
	c.observe(bdgateway.WorkspaceAnswer{Hash: "new"})
	if pending, _ := c.take(); !pending {
		t.Fatal("a foreign differing hash recorded no drift, want one")
	}
}

// TestWorkspaceDriftPushIsMarkedOwn: every push the loop sends carries the
// own-push mark, so the gateway's answer to it is observed as the loop's own and
// is not drift. Checked on the start push and a tick push.
func TestWorkspaceDriftPushIsMarkedOwn(t *testing.T) {
	t.Parallel()
	r := startDriftRun(t)
	r.clk.Advance(config.DefaultWorkspaceReconcileInterval)
	if got := awaitPush(t, r.pusher.calls); got != 2 {
		t.Fatalf("push = call %d, want the tick push 2", got)
	}
	for i, own := range r.pusher.own {
		if !own {
			t.Errorf("push %d carried no own-push mark, so its answer would read as drift", i+1)
		}
	}
}

// TestWorkspaceDriftAbsentFloor: absent refusals push at most once per
// workspaceAbsentFloor. An absent refusal a minute after the last push pushes at
// once. A second one straight after waits for the floor, though the drift gap ended
// long before, and then pushes once.
func TestWorkspaceDriftAbsentFloor(t *testing.T) {
	t.Parallel()
	r := startDriftRun(t)
	r.clk.Advance(time.Minute)
	r.d.workspaceDrift.observe(bdgateway.WorkspaceAnswer{Absent: true})
	if got := awaitPush(t, r.pusher.calls); got != 2 {
		t.Fatalf("push = call %d, want the absent push 2", got)
	}
	r.d.workspaceDrift.observe(bdgateway.WorkspaceAnswer{Absent: true})
	awaitWaits(t, r.clk.FakeClock, 2)
	r.clk.Advance(workspaceDriftGap)
	// The drift gap has ended but the floor has not: the reconcile still holds its
	// deferred wait, so it has not pushed.
	awaitWaits(t, r.clk.FakeClock, 2)
	r.clk.Advance(workspaceAbsentFloor - workspaceDriftGap)
	if got := awaitPush(t, r.pusher.calls); got != 3 {
		t.Fatalf("push = call %d, want the floored absent push 3", got)
	}
	if got := r.at[2].Sub(r.at[1]); got != workspaceAbsentFloor {
		t.Errorf("second absent push at %s after the first, want %s", got, workspaceAbsentFloor)
	}
}

// TestWorkspaceDriftFailedPushFloor: a push that fails stores nothing, so the
// next probe sees the same drift. The next drift push waits workspaceAbsentFloor
// from the failed push, not workspaceDriftGap, and a push that succeeds clears
// the floor so the next drift pushes after the gap alone.
func TestWorkspaceDriftFailedPushFloor(t *testing.T) {
	t.Parallel()
	r := startDriftRun(t)
	r.pusher.fail = func(call int) error {
		if call == 2 {
			return context.DeadlineExceeded
		}
		return nil
	}
	r.clk.Advance(time.Minute)
	r.d.workspaceDrift.observe(bdgateway.WorkspaceAnswer{Hash: "other"})
	if got := awaitPush(t, r.pusher.calls); got != 2 {
		t.Fatalf("push = call %d, want the failing push 2", got)
	}
	r.d.workspaceDrift.observe(bdgateway.WorkspaceAnswer{Hash: "other"})
	awaitWaits(t, r.clk.FakeClock, 2)
	r.clk.Advance(workspaceDriftGap)
	// The drift gap has ended but the floor from the failed push has not.
	awaitWaits(t, r.clk.FakeClock, 2)
	r.clk.Advance(workspaceAbsentFloor - workspaceDriftGap)
	if got := awaitPush(t, r.pusher.calls); got != 3 {
		t.Fatalf("push = call %d, want the floored push 3", got)
	}
	if got := r.at[2].Sub(r.at[1]); got != workspaceAbsentFloor {
		t.Errorf("push after a failed push came %s later, want %s", got, workspaceAbsentFloor)
	}
	// Push 3 succeeded, so the floor is cleared: the next drift needs only the gap.
	r.d.workspaceDrift.observe(bdgateway.WorkspaceAnswer{Hash: "another"})
	awaitWaits(t, r.clk.FakeClock, 2)
	r.clk.Advance(workspaceDriftGap)
	if got := awaitPush(t, r.pusher.calls); got != 4 {
		t.Fatalf("push = call %d, want the unfloored push 4", got)
	}
	if got := r.at[3].Sub(r.at[2]); got != workspaceDriftGap {
		t.Errorf("push after a good push came %s later, want %s", got, workspaceDriftGap)
	}
}

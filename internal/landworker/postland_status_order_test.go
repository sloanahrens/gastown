package landworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

const (
	// newerSHA is a commit landed after redSHA in every ordering test.
	newerSHA = "9999999999999999999999999999999999999999"
)

// orderedStatus is a status-mode runner over the real red-main owner and its
// revert harness, with commits that have an order: a verdict for an earlier
// commit is an ancestor of a later one's.
type orderedStatus struct {
	*revertHarness
	run   *PostLandRunner
	reads map[string]*fakeStatusReads
	clk   *manualClock
	order []string

	mu       sync.Mutex
	greens   int
	reds     int
	redCmds  []string
	redRes   []PostLandResult
	logs     []string
	ancErr   error
	runCalls []string
	runFn    func(ctx context.Context, cmd string, pl PostLand) PostLandResult
}

// newOrderedStatus builds the runner. command is merge_queue.post_land_command
// ("" for none); order lists the commits oldest first (greenSHA is the oldest).
func newOrderedStatus(t *testing.T, command string, order ...string) *orderedStatus {
	t.Helper()
	h := newRevertHarness(t, greenSHA)
	o := &orderedStatus{
		revertHarness: h,
		reads:         map[string]*fakeStatusReads{},
		clk:           &manualClock{now: statusT0},
		order:         append([]string{greenSHA}, order...),
	}
	o.run = &PostLandRunner{
		Rig:           "gastown",
		StatusContext: func() string { return statusContext },
		Command:       func() string { return command },
		Status: func(ctx context.Context, pl PostLand) (*forgejo.CombinedStatus, error) {
			return byCommit(o.reads)(ctx, pl)
		},
		State:      h.state,
		Clock:      o.clk,
		Timeout:    time.Hour,
		Beads:      h.bd,
		IsAncestor: o.isAncestor,
		Run: func(ctx context.Context, cmd string, pl PostLand) PostLandResult {
			o.mu.Lock()
			o.runCalls = append(o.runCalls, cmd+"@"+pl.Commit)
			fn := o.runFn
			o.mu.Unlock()
			if fn == nil {
				return PostLandResult{}
			}
			return fn(ctx, cmd, pl)
		},
		Logf: func(format string, args ...any) {
			o.mu.Lock()
			o.logs = append(o.logs, strings.TrimSpace(fmt.Sprintf(format, args...)))
			o.mu.Unlock()
		},
		OnGreen: func(ctx context.Context, cmd string, pl PostLand, res PostLandResult) {
			o.mu.Lock()
			o.greens++
			o.mu.Unlock()
			h.r.Green(ctx, cmd, pl, res)
		},
		OnRed: func(ctx context.Context, cmd string, pl PostLand, res PostLandResult) {
			o.mu.Lock()
			o.reds++
			o.redCmds = append(o.redCmds, cmd)
			o.redRes = append(o.redRes, res)
			o.mu.Unlock()
			h.r.Red(ctx, cmd, pl, res)
		},
	}
	return o
}

func (o *orderedStatus) isAncestor(ancestor, descendant string) (bool, error) {
	if o.ancErr != nil {
		return false, o.ancErr
	}
	ia, id := -1, -1
	for i, c := range o.order {
		if c == ancestor {
			ia = i
		}
		if c == descendant {
			id = i
		}
	}
	if ia < 0 || id < 0 {
		return false, errors.New("unknown commit")
	}
	return ia < id, nil
}

// status scripts a commit's status reads.
func (o *orderedStatus) status(commit string, responses ...*forgejo.CombinedStatus) {
	reads := &fakeStatusReads{}
	for _, cs := range responses {
		reads.responses = append(reads.responses, fakeStatusRead{status: cs})
	}
	o.reads[commit] = reads
}

func (o *orderedStatus) logText() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Join(o.logs, "\n")
}

func (o *orderedStatus) counts() (greens, reds int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.greens, o.reds
}

// poll runs one pass, then waits for any rerun it started.
func (o *orderedStatus) poll(ctx context.Context) {
	o.run.PollPending(ctx)
	o.run.Wait()
}

// later moves the clock past one poll interval.
func (o *orderedStatus) later() { o.clk.Advance(PostLandStatusPollInterval + time.Second) }

func green() *forgejo.CombinedStatus { return combinedFor(statusContext, forgejo.StateSuccess) }
func red() *forgejo.CombinedStatus   { return combinedFor(statusContext, forgejo.StateFailure) }
func pending() *forgejo.CombinedStatus {
	return combinedFor(statusContext, forgejo.StatePending)
}

// receive waits for ch, failing the test instead of hanging it when the event
// never comes (a rerun that was never started).
func receive(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func landOf(bead, commit string) PostLand {
	return PostLand{BeadID: bead, Commit: commit, Target: "main"}
}

// TestPostLandStatusOlderVerdictMovesNothing: CI reports in its own order, so a
// verdict for a commit older than the newest verdict must not lower LastGreen,
// promote, file a red-main bead or build a revert (gt-gg9hv.7.1; owl gt-f33vfi).
func TestPostLandStatusOlderVerdictMovesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		older *forgejo.CombinedStatus
	}{
		{"green after a newer green", green()},
		{"red after a newer green", red()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newOrderedStatus(t, "", redSHA, newerSHA)
			o.status(redSHA, pending(), tc.older)
			o.status(newerSHA, green())
			o.run.Trigger(ctx, landOf("gt-cul", redSHA))
			o.run.Trigger(ctx, landOf("gt-new", newerSHA))

			o.poll(ctx) // the newer commit is green while the older one is pending
			o.later()
			o.poll(ctx) // the older commit's verdict arrives

			st, _ := o.state.Load()
			if st.LastGreen != newerSHA || st.LastRun != newerSHA {
				t.Fatalf("LastGreen %s LastRun %s after the older verdict, want both at the newer commit %s", st.LastGreen, st.LastRun, newerSHA)
			}
			if greens, reds := o.counts(); greens != 1 || reds != 0 {
				t.Fatalf("green %d red %d, want only the newer commit's green", greens, reds)
			}
			if open := o.open(t); len(open) != 0 {
				t.Fatalf("red-main beads open after an older verdict: %v", open)
			}
			if len(o.reverts) != 0 {
				t.Fatalf("reverts built after an older verdict: %v", o.reverts)
			}
			if !strings.Contains(o.logText(), "ignored: a verdict for a newer commit stands") {
				t.Fatalf("the ignored verdict was not logged:\n%s", o.logText())
			}
			if left := pendingOf(t, o.state); len(left) != 0 {
				t.Fatalf("pending = %+v, want the decided entries gone", left)
			}
		})
	}
}

// TestPostLandStatusOrderingNeverBlocksWhatItCannotRead: an ancestry that cannot
// be read, or none wired, is "not older": the verdict fires as it always did.
func TestPostLandStatusOrderingNeverBlocksWhatItCannotRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(o *orderedStatus)
	}{
		{"ancestry error", func(o *orderedStatus) { o.ancErr = errors.New("git: bad object") }},
		{"no reader wired", func(o *orderedStatus) { o.run.IsAncestor = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newOrderedStatus(t, "", redSHA, newerSHA)
			tc.setup(o)
			o.status(redSHA, pending(), green())
			o.status(newerSHA, green())
			o.run.Trigger(ctx, landOf("gt-cul", redSHA))
			o.run.Trigger(ctx, landOf("gt-new", newerSHA))
			o.poll(ctx)
			o.later()
			o.poll(ctx)
			if greens, _ := o.counts(); greens != 2 {
				t.Fatalf("green %d, want both verdicts to fire when order cannot be read", greens)
			}
		})
	}
}

// TestPostLandStatusRedAloneNeverReverts: a red status names no failing unit, so
// it never reverts on its own. With no command to rerun it files the red-main
// bead and states why no revert follows, for a docs-only landing and a Go
// landing alike (policy, gt-gg9hv.7.1).
func TestPostLandStatusRedAloneNeverReverts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		changed []string
	}{
		{"docs-only landing", []string{"docs/reference.md"}},
		{"Go landing", []string{"internal/a/a.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newOrderedStatus(t, "", redSHA)
			o.changed = tc.changed
			o.status(redSHA, red())
			o.run.Trigger(ctx, landOf("gt-cul", redSHA))
			o.poll(ctx)

			if _, reds := o.counts(); reds != 1 {
				t.Fatalf("OnRed %d times, want once", reds)
			}
			if len(o.reverts) != 0 || len(o.revertBeads(t)) != 0 {
				t.Fatalf("a red status alone built a revert: %v", o.reverts)
			}
			if open := o.open(t); len(open) != 1 {
				t.Fatalf("red-main beads open = %d, want the one bead for the red status", len(open))
			}
			if s := o.lastStatus(); !strings.Contains(s, "no revert: no post_land_command") {
				t.Fatalf("status %q, want it to say no revert and why", s)
			}
		})
	}
}

// TestPostLandStatusRedIsRerunBeforeAnyRevert: with a post_land_command a red
// status reruns the check at the landed commit, and only a rerun that fails again
// reaches the rerun-and-attribute path that can revert.
func TestPostLandStatusRedIsRerunBeforeAnyRevert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const cmd = "make test-slow"

	t.Run("a rerun that passes files the bead and reverts nothing", func(t *testing.T) {
		t.Parallel()
		o := newOrderedStatus(t, cmd, redSHA)
		o.runFn = func(context.Context, string, PostLand) PostLandResult { return PostLandResult{ExitCode: 0} }
		o.status(redSHA, red())
		o.run.Trigger(ctx, landOf("gt-cul", redSHA))
		o.poll(ctx)
		if len(o.runCalls) != 1 || o.runCalls[0] != cmd+"@"+redSHA {
			t.Fatalf("reruns = %v, want one rerun of the check at the landed commit", o.runCalls)
		}
		if len(o.reverts) != 0 {
			t.Fatalf("reverted after a rerun that passed: %v", o.reverts)
		}
		if len(o.open(t)) != 1 || !strings.Contains(o.lastStatus(), "no revert: the rerun of the post-land check passed") {
			t.Fatalf("open %v status %q, want the bead and the reason", o.open(t), o.lastStatus())
		}
	})

	t.Run("a rerun that cannot run files the bead and reverts nothing", func(t *testing.T) {
		t.Parallel()
		o := newOrderedStatus(t, cmd, redSHA)
		o.runFn = func(context.Context, string, PostLand) PostLandResult {
			return PostLandResult{Err: errors.New("no slot")}
		}
		o.status(redSHA, red())
		o.run.Trigger(ctx, landOf("gt-cul", redSHA))
		o.poll(ctx)
		if len(o.reverts) != 0 || !strings.Contains(o.lastStatus(), "no revert: the rerun of the post-land check could not run") {
			t.Fatalf("reverts %v status %q", o.reverts, o.lastStatus())
		}
	})

	t.Run("a rerun that fails again and names the landing's package reverts it once", func(t *testing.T) {
		t.Parallel()
		o := newOrderedStatus(t, cmd, redSHA)
		o.runFn = func(context.Context, string, PostLand) PostLandResult {
			return PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})}
		}
		o.status(redSHA, red())
		o.run.Trigger(ctx, landOf("gt-cul", redSHA))
		o.poll(ctx)
		if len(o.reverts) != 1 {
			t.Fatalf("reverts = %v, want the landing reverted once", o.reverts)
		}
		if o.redCmds[0] != cmd {
			t.Fatalf("OnRed command %q, want the rerun's own command %q", o.redCmds[0], cmd)
		}
	})

	t.Run("a docs-only landing is not reverted for a failure its diff cannot explain", func(t *testing.T) {
		t.Parallel()
		o := newOrderedStatus(t, cmd, redSHA)
		o.changed = []string{"docs/reference.md"}
		o.runFn = func(context.Context, string, PostLand) PostLandResult {
			return PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})}
		}
		o.status(redSHA, red())
		o.run.Trigger(ctx, landOf("gt-cul", redSHA))
		o.poll(ctx)
		if len(o.reverts) != 0 || o.diffCalls != 1 || !strings.Contains(o.lastStatus(), "no revert:") {
			t.Fatalf("reverts %v diff reads %d status %q: want the diff read once and no revert", o.reverts, o.diffCalls, o.lastStatus())
		}
	})
}

// TestPostLandStatusRerunDoesNotHoldThePass: the rerun takes as long as the
// check, so the pass that sees the red status returns, the entry stays in the
// store until the rerun ends, the commit is read and decided once, and an
// upgrade restart is held for the rerun and no longer.
func TestPostLandStatusRerunDoesNotHoldThePass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := newOrderedStatus(t, "make test-slow", redSHA)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	o.runFn = func(context.Context, string, PostLand) PostLandResult {
		once.Do(func() { close(started) })
		<-release
		return PostLandResult{ExitCode: 0}
	}
	var busy atomic.Int32
	o.run.Busy = func(b bool) {
		if b {
			busy.Add(1)
		} else {
			busy.Add(-1)
		}
	}
	o.status(redSHA, red())
	o.run.Trigger(ctx, landOf("gt-cul", redSHA))

	done := make(chan struct{})
	go func() { o.run.PollPending(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PollPending waited for the rerun")
	}
	receive(t, started, "the rerun to start")
	if busy.Load() != 1 {
		t.Fatalf("busy = %d while the rerun runs, want an upgrade restart held", busy.Load())
	}
	if left := pendingOf(t, o.state); len(left) != 1 {
		t.Fatalf("pending = %+v, want the entry kept until the rerun ends", left)
	}
	o.later()
	o.run.PollPending(ctx)
	if got := o.reads[redSHA].count(); got != 1 {
		t.Fatalf("status read %d times while its rerun runs, want once", got)
	}

	close(release)
	o.run.Wait()
	if busy.Load() != 0 {
		t.Fatalf("busy = %d after the rerun, want released", busy.Load())
	}
	if _, reds := o.counts(); reds != 1 {
		t.Fatalf("OnRed %d times, want once", reds)
	}
	o.run.PollPending(ctx)
	if left := pendingOf(t, o.state); len(left) != 0 {
		t.Fatalf("pending = %+v after the rerun, want it finished", left)
	}
	if _, reds := o.counts(); reds != 1 {
		t.Fatalf("OnRed %d times after the cleanup pass, want still once", reds)
	}
}

// TestPostLandStatusStopDuringARerunLeavesTheEntry: a daemon stop that cuts the
// rerun short decides nothing and keeps the entry, so the restarted runner reads
// the status again and decides it then.
func TestPostLandStatusStopDuringARerunLeavesTheEntry(t *testing.T) {
	t.Parallel()
	o := newOrderedStatus(t, "make test-slow", redSHA)
	ctx, stop := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	o.runFn = func(ctx context.Context, _ string, _ PostLand) PostLandResult {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return PostLandResult{ExitCode: -1}
	}
	o.status(redSHA, red())
	o.run.Trigger(ctx, landOf("gt-cul", redSHA))
	o.run.PollPending(ctx)
	receive(t, started, "the rerun to start")
	stop()
	o.run.Wait()

	if _, reds := o.counts(); reds != 0 {
		t.Fatalf("OnRed %d times for a rerun the stop cut short, want none", reds)
	}
	if left := pendingOf(t, o.state); len(left) != 1 {
		t.Fatalf("pending = %+v, want the entry kept for the restart", left)
	}

	// The restarted runner reads the status again and decides it.
	o.runFn = func(context.Context, string, PostLand) PostLandResult { return PostLandResult{ExitCode: 0} }
	o.later()
	o.poll(context.Background())
	if _, reds := o.counts(); reds != 1 {
		t.Fatalf("OnRed %d times after the restart, want once", reds)
	}
}

// TestPostLandStatusRerunChecksTheOrderAgain: a newer verdict can land while a
// rerun runs; the older commit's red then moves nothing.
func TestPostLandStatusRerunChecksTheOrderAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := newOrderedStatus(t, "make test-slow", redSHA, newerSHA)
	o.runFn = func(ctx context.Context, cmd string, _ PostLand) PostLandResult {
		// While the older commit's rerun runs, the newer commit goes green.
		o.r.Green(ctx, cmd, landOf("gt-new", newerSHA), PostLandResult{})
		return PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})}
	}
	o.status(redSHA, red())
	o.run.Trigger(ctx, landOf("gt-cul", redSHA))
	o.poll(ctx)
	if _, reds := o.counts(); reds != 0 {
		t.Fatalf("OnRed %d times, want none: a newer verdict stood when the rerun ended", reds)
	}
	if len(o.open(t)) != 0 || len(o.reverts) != 0 {
		t.Fatalf("beads %v reverts %v after an older red", o.open(t), o.reverts)
	}
}

// TestPostLandStatusLateVerdictIsLoggedAndClearsTheItem: CI that reports after
// post_land_timeout is not acted on, but it is said, and its timeout item goes.
func TestPostLandStatusLateVerdictIsLoggedAndClearsTheItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const timeout = 17 * time.Minute
	for _, late := range []*forgejo.CombinedStatus{green(), red()} {
		o := newOrderedStatus(t, "", redSHA)
		o.run.Timeout = timeout
		reads := &fakeStatusReads{responses: []fakeStatusRead{{status: pending()}}}
		o.reads[redSHA] = reads
		o.run.Trigger(ctx, landOf("gt-cul", redSHA))
		o.poll(ctx)

		o.clk.Advance(timeout)
		o.poll(ctx)
		alarms, err := o.run.Alarms(o.clk.Now())
		if err != nil || len(alarms.Unknown) != 1 {
			t.Fatalf("alarms %+v (%v) at the timeout, want one item", alarms, err)
		}
		before := reads.count()

		// A timed-out entry is read at the slow rate, not every poll.
		o.later()
		o.poll(ctx)
		if got := reads.count(); got != before {
			t.Fatalf("a timed-out entry was read %d more times inside the late interval", got-before)
		}

		reads.mu.Lock()
		reads.responses = []fakeStatusRead{{status: late}}
		reads.mu.Unlock()
		o.clk.Advance(PostLandLateReadInterval)
		o.poll(ctx)

		if !strings.Contains(o.logText(), "LATE verdict at") || !strings.Contains(o.logText(), "after post_land_timeout (17m0s)") {
			t.Fatalf("a verdict after the timeout was not logged:\n%s", o.logText())
		}
		if greens, reds := o.counts(); greens != 0 || reds != 0 {
			t.Fatalf("a late verdict was acted on: green %d red %d", greens, reds)
		}
		if alarms, _ := o.run.Alarms(o.clk.Now()); len(alarms.Unknown) != 0 {
			t.Fatalf("the timeout item stands after the late verdict: %+v", alarms.Unknown)
		}
		if left := pendingOf(t, o.state); len(left) != 0 {
			t.Fatalf("pending = %+v, want the entry gone", left)
		}
	}
}

// TestPostLandStatusDuplicateTriggerKeepsOneEntry: a landing recorded twice (a
// retried trigger) is one pending entry, so it is read and decided once.
func TestPostLandStatusDuplicateTriggerKeepsOneEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := newOrderedStatus(t, "", redSHA)
	o.status(redSHA, pending())
	o.run.Trigger(ctx, landOf("gt-cul", redSHA))
	o.clk.Advance(time.Minute)
	o.run.Trigger(ctx, landOf("gt-cul", redSHA))
	left := pendingOf(t, o.state)
	if len(left) != 1 {
		t.Fatalf("pending = %d entries after a second trigger of one commit, want 1", len(left))
	}
	if !left[0].Since.Equal(statusT0) {
		t.Fatalf("Since = %v, want the first trigger's time: the timeout must not restart", left[0].Since)
	}
}

// TestPostLandStatusReadsTheLeastRecentlyReadFirst: a pass that cannot read
// every entry starts with the one read longest ago.
func TestPostLandStatusReadsTheLeastRecentlyReadFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := newOrderedStatus(t, "", redSHA, newerSHA)
	var order []string
	var mu sync.Mutex
	o.run.Status = func(_ context.Context, pl PostLand) (*forgejo.CombinedStatus, error) {
		mu.Lock()
		order = append(order, pl.Commit)
		mu.Unlock()
		return pending(), nil
	}
	o.run.Trigger(ctx, landOf("gt-cul", redSHA))
	o.run.Trigger(ctx, landOf("gt-new", newerSHA))
	// The newer commit was read last time, the older one never: the older goes first.
	st, _ := o.state.Load()
	st.PostLandPending[1].LastRead = statusT0.Add(-time.Hour)
	st.PostLandPending[0].LastRead = statusT0.Add(-time.Minute)
	if err := o.state.Save(st); err != nil {
		t.Fatal(err)
	}
	o.poll(ctx)
	if len(order) != 2 || order[0] != newerSHA {
		t.Fatalf("read order %v, want the entry read longest ago (%s) first", order, newerSHA)
	}
}

// TestPostLandStatusRedBeadNamesTheStatus: the red-main bead for a red status
// says the status was red and why nothing was rerun or reverted, not that a
// command exited.
func TestPostLandStatusRedBeadNamesTheStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := newOrderedStatus(t, "", redSHA)
	o.status(redSHA, &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{
		{Context: statusContext, Status: forgejo.StateFailure, Description: "runner evicted"},
	}})
	o.run.Trigger(ctx, landOf("gt-cul", redSHA))
	o.poll(ctx)
	open := o.open(t)
	if len(open) != 1 {
		t.Fatalf("open beads %v", open)
	}
	for _, is := range open {
		if strings.Contains(is.Description, "exited") || !strings.Contains(is.Description, "post-land status") ||
			!strings.Contains(is.Description, "runner evicted") || !strings.Contains(is.Description, "no revert") {
			t.Fatalf("bead description does not describe a red status:\n%s", is.Description)
		}
	}
}

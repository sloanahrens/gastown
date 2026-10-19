package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// newScheduledOverrunDaemon is newOverrunDaemon with scheduled_stop_after set
// to limit ("" leaves the default).
func newScheduledOverrunDaemon(t *testing.T, limit string, dryRun bool) (*Daemon, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverrunGuard: &OverrunGuardConfig{
		Enabled: true, DryRun: dryRun, ScheduledStopAfter: limit,
	}}}
	return &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		patrolConfig: cfg,
		logger:       log.New(&buf, "", 0),
	}, &buf
}

func TestOverrunScheduledStopAfter(t *testing.T) {
	t.Parallel()
	with := func(s string) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverrunGuard: &OverrunGuardConfig{ScheduledStopAfter: s}}}
	}
	tests := []struct {
		name string
		cfg  *DaemonPatrolConfig
		want time.Duration
	}{
		{"nil config", nil, 60 * time.Minute},
		{"nil patrols", &DaemonPatrolConfig{}, 60 * time.Minute},
		{"unset", with(""), 60 * time.Minute},
		{"configured", with("90m"), 90 * time.Minute},
		{"zero means never", with("0"), 0},
		{"zero seconds means never", with("0s"), 0},
		{"unparseable falls back", with("an hour"), 60 * time.Minute},
		{"negative falls back", with("-5m"), 60 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := overrunScheduledStopAfter(tt.cfg); got != tt.want {
				t.Errorf("overrunScheduledStopAfter = %v, want %v", got, tt.want)
			}
		})
	}
}

// A scheduled seat at 59m is left alone: no nudge, no stop.
func TestRunOverrunGuard_ScheduledLeftAloneBeforeLimit(t *testing.T) {
	t.Parallel()
	d, buf := newScheduledOverrunDaemon(t, "", false)
	f := newOverrunFake(59*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})

	d.runOverrunGuard(f.seams())

	if len(f.calls) != 0 || len(f.nudges) != 0 || len(f.comments) != 0 {
		t.Errorf("seat acted on at 59m: calls %v, nudges %v, comments %v", f.calls, f.nudges, f.comments)
	}
	if buf.Len() != 0 {
		t.Errorf("log at 59m:\n%s", buf.String())
	}
}

// At 60m the session is stopped once, with the pause marker first, one log
// line and one bead comment, and nothing else: the bead is not handed back,
// checkpointed, pushed or released.
func TestRunOverrunGuard_ScheduledStoppedAtLimit(t *testing.T) {
	t.Parallel()
	d, buf := newScheduledOverrunDaemon(t, "", false)
	f := newOverrunFake(60*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})

	d.runOverrunGuard(f.seams())

	want := []string{"pause gastown/capable", "stop gastown/capable", "comment gastown/capable", "close gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
	if len(f.closed) != 1 || !strings.HasPrefix(f.closed[0], "gt-r71dd: stopped by the overrun guard after 1h0m0s") ||
		!strings.Contains(f.closed[0], "scheduled_stop_after 1h0m0s") {
		t.Errorf("closed = %q, want the run bead closed with the limit in the reason", f.closed)
	}
	if !slices.Equal(f.agentBeadsReset, []string{"gt-r71dd"}) {
		t.Errorf("agent beads reset = %v, want the run's", f.agentBeadsReset)
	}
	if len(f.nudges) != 0 || len(f.handedBack) != 0 {
		t.Errorf("nudges %v, hand-backs %v; want none", f.nudges, f.handedBack)
	}
	wantLog := "overrun_guard: stopped scheduled gastown/capable on gt-r71dd after 1h0m0s"
	if n := strings.Count(buf.String(), wantLog); n != 1 {
		t.Errorf("log has %d %q lines, want 1:\n%s", n, wantLog, buf.String())
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "gt-r71dd: ") ||
		!strings.Contains(f.comments[0], "scheduled_stop_after (1h0m0s)") {
		t.Errorf("comments = %q, want one naming scheduled_stop_after (1h0m0s)", f.comments)
	}
	st, err := readOverrunState(d.config.TownRoot, "gt-r71dd")
	if err != nil || st.StoppedAt.IsZero() || !st.Scheduled || st.CommentedAt.IsZero() || st.ClosedAt.IsZero() {
		t.Errorf("state = %+v (err %v), want a finished scheduled stop that commented and closed", st, err)
	}
	if want := "overrun_guard: released scheduled gastown/capable: run bead gt-r71dd closed, seat retired"; !strings.Contains(buf.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, buf.String())
	}

	// The next tick finds the seat again (the fake keeps listing it) and does
	// nothing: the stop ran for this sling.
	f.calls, f.comments = nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.calls) != 0 || len(f.comments) != 0 {
		t.Errorf("second tick acted again: calls %v, comments %v", f.calls, f.comments)
	}
}

// The configured limit replaces the default.
func TestRunOverrunGuard_ScheduledHonoursConfiguredLimit(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		limit   string
		elapsed time.Duration
		stopped bool
	}{
		{"30m", 29 * time.Minute, false},
		{"30m", 30 * time.Minute, true},
		{"2h", 90 * time.Minute, false},
		{"2h", 2 * time.Hour, true},
	} {
		d, _ := newScheduledOverrunDaemon(t, tt.limit, false)
		f := newOverrunFake(tt.elapsed, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})

		d.runOverrunGuard(f.seams())

		if got := len(f.stops) == 1; got != tt.stopped {
			t.Errorf("limit %s at %v: stopped = %v, want %v", tt.limit, tt.elapsed, got, tt.stopped)
		}
	}
}

// A zero scheduled_stop_after keeps the old exemption: never nudged, never
// stopped, however long the run.
func TestRunOverrunGuard_ScheduledZeroNeverStops(t *testing.T) {
	t.Parallel()
	for _, limit := range []string{"0", "0s"} {
		d, buf := newScheduledOverrunDaemon(t, limit, false)
		f := newOverrunFake(72*time.Hour, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})

		d.runOverrunGuard(f.seams())

		if len(f.calls) != 0 || len(f.nudges) != 0 || len(f.comments) != 0 {
			t.Errorf("limit %q: seat acted on: calls %v, nudges %v, comments %v", limit, f.calls, f.nudges, f.comments)
		}
		if buf.Len() != 0 {
			t.Errorf("limit %q: log:\n%s", limit, buf.String())
		}
	}
}

// A seat that is not on a scheduled sling still follows the 12 and 20 minute
// rule, whatever scheduled_stop_after says, and is stopped the full way
// (checkpoint, hand-back) while the scheduled seat beside it is not.
func TestRunOverrunGuard_ScheduledLimitDoesNotTouchOtherSeats(t *testing.T) {
	t.Parallel()
	d, _ := newScheduledOverrunDaemon(t, "0", false)
	f := newOverrunFake(21*time.Minute,
		overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true},
		overrunSeat{Rig: "gastown", Polecat: "nux", Bead: "gt-plain"},
	)

	d.runOverrunGuard(f.seams())

	if len(f.handedBack) != 1 || !strings.HasPrefix(f.handedBack[0], "gt-plain ") {
		t.Errorf("hand-backs = %v, want gt-plain only", f.handedBack)
	}
	if len(f.comments) != 0 {
		t.Errorf("a non-scheduled stop added a scheduled-stop comment: %q", f.comments)
	}
	for _, c := range f.calls {
		if strings.HasSuffix(c, "gastown/capable") {
			t.Errorf("scheduled seat was acted on: %s", c)
		}
	}
}

// A scheduled seat is never nudged, even between the 12 and 20 minute marks
// or after a nudge was recorded for an earlier run.
func TestRunOverrunGuard_ScheduledNeverNudged(t *testing.T) {
	t.Parallel()
	for _, elapsed := range []time.Duration{12 * time.Minute, 30 * time.Minute, 59 * time.Minute} {
		d, _ := newScheduledOverrunDaemon(t, "", false)
		f := newOverrunFake(elapsed, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})

		d.runOverrunGuard(f.seams())

		if len(f.nudges) != 0 || len(f.calls) != 0 {
			t.Errorf("after %v: nudges %v, calls %v; want none", elapsed, f.nudges, f.calls)
		}
	}
}

func TestRunOverrunGuard_ScheduledDryRun(t *testing.T) {
	t.Parallel()
	d, buf := newScheduledOverrunDaemon(t, "", true)
	f := newOverrunFake(61*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})

	d.runOverrunGuard(f.seams())

	if len(f.calls) != 0 || len(f.comments) != 0 {
		t.Errorf("dry run acted: calls %v, comments %v", f.calls, f.comments)
	}
	if !strings.Contains(buf.String(), "would stop scheduled gastown/capable on gt-r71dd after 1h1m0s") {
		t.Errorf("log lacks the would-stop line:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-r71dd"); !st.StopBeganAt.IsZero() {
		t.Errorf("dry run wrote stop state %+v", st)
	}
}

// A session that will not stop clears the pause again, logs, adds no comment,
// and is tried again from the top on the next tick.
func TestRunOverrunGuard_ScheduledFailedStopRetriesNextTick(t *testing.T) {
	t.Parallel()
	d, buf := newScheduledOverrunDaemon(t, "", false)
	f := newOverrunFake(61*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})
	f.failOn = map[string]error{"stop": errors.New("tmux refused")}

	d.runOverrunGuard(f.seams())

	want := []string{"pause gastown/capable", "stop gastown/capable", "clear-pause gastown/capable"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("first tick calls = %v, want %v", f.calls, want)
	}
	if !strings.Contains(buf.String(), "stop scheduled gastown/capable on gt-r71dd: session stop failed: tmux refused") {
		t.Errorf("failure not logged:\n%s", buf.String())
	}
	if len(f.comments) != 0 {
		t.Errorf("comment added for a stop that failed: %q", f.comments)
	}

	f.calls, f.failOn = nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	want = []string{"pause gastown/capable", "stop gastown/capable", "comment gastown/capable", "close gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("second tick calls = %v, want %v", f.calls, want)
	}
	if len(f.comments) != 1 {
		t.Errorf("comments = %q, want exactly one", f.comments)
	}
}

// A scheduled stop resumed with StopBeganAt set and the session not recorded
// as stopped (the pause marker could not be cleared) re-checks the seat before
// stopping the session: a seat taken back by an operator keeps its new session.
func TestRunOverrunGuard_ScheduledResumedBeforeSessionStopKeepsANewSession(t *testing.T) {
	t.Parallel()
	d, buf := newScheduledOverrunDaemon(t, "", false)
	f := newOverrunFake(61*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})
	f.failOn = map[string]error{"stop": errors.New("tmux refused"), "clear-pause": errors.New("disk full")}
	d.runOverrunGuard(f.seams())

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.lapsed = "the pause marker is gone"
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	if want := []string{"recheck gastown/capable"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want only the re-check (no stop, comment or close)", f.calls)
	}
	if !strings.Contains(buf.String(), "abandoning the stop") {
		t.Errorf("log lacks the abandon line:\n%s", buf.String())
	}
}

// A comment that fails after the session is gone is retried from the saved
// state on the next tick, when the seat is no longer listed; the resume goes on
// to close the run bead and release the seat, and never hands the bead back.
func TestRunOverrunGuard_ScheduledFailedCommentResumes(t *testing.T) {
	t.Parallel()
	d, buf := newScheduledOverrunDaemon(t, "", false)
	f := newOverrunFake(61*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})
	f.failOn = map[string]error{"comment": errors.New("bd down")}

	d.runOverrunGuard(f.seams())

	if !strings.Contains(buf.String(), "comment failed (1 of 5): bd down") {
		t.Errorf("failure not logged:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-r71dd"); !st.StoppedAt.IsZero() || !st.Scheduled || st.SessionStoppedAt.IsZero() {
		t.Errorf("state after the failed comment = %+v, want a pending scheduled stop with its session stopped", st)
	}

	// The session is gone, so the seat list no longer has the seat.
	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	want := []string{"recheck gastown/capable", "comment gastown/capable", "close gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("resume calls = %v, want %v", f.calls, want)
	}
	if len(f.comments) != 1 || len(f.handedBack) != 0 {
		t.Errorf("comments %q, hand-backs %v; want one comment and no hand-back", f.comments, f.handedBack)
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-r71dd"); st.StoppedAt.IsZero() {
		t.Errorf("stop still pending after the comment: %+v", st)
	}
}

// A comment that never lands is given up on after five tries; the session
// stays stopped and the stop is over.
func TestRunOverrunGuard_ScheduledCommentGivenUp(t *testing.T) {
	t.Parallel()
	d, buf := newScheduledOverrunDaemon(t, "", false)
	f := newOverrunFake(61*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})
	f.failOn = map[string]error{"comment": errors.New("bd down")}

	d.runOverrunGuard(f.seams())
	f.seats = nil
	for range overrunCommentMaxAttempts {
		f.now = f.now.Add(time.Minute)
		d.runOverrunGuard(f.seams())
	}

	if !strings.Contains(buf.String(), "giving up on the comment") {
		t.Errorf("give-up not logged:\n%s", buf.String())
	}
	st, _ := readOverrunState(d.config.TownRoot, "gt-r71dd")
	if st.StoppedAt.IsZero() || st.CommentFailures != overrunCommentMaxAttempts {
		t.Errorf("state = %+v, want a finished stop after %d failed comments", st, overrunCommentMaxAttempts)
	}
	if n := strings.Count(strings.Join(f.calls, "\n"), "stop gastown/capable"); n != 1 {
		t.Errorf("session stopped %d times, want 1", n)
	}
}

// A seat that began gt done after the seat list was read is left running.
func TestRunOverrunGuard_ScheduledSubmittedBeforeStop(t *testing.T) {
	t.Parallel()
	d, _ := newScheduledOverrunDaemon(t, "", false)
	f := newOverrunFake(61*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})
	f.submitted = true

	d.runOverrunGuard(f.seams())

	if len(f.calls) != 0 || len(f.comments) != 0 {
		t.Errorf("submitted seat acted on: calls %v, comments %v", f.calls, f.comments)
	}
}

// A close that fails after the comment landed is retried on the next tick without
// repeating the comment, and the seat is not released until the bead is closed.
func TestRunOverrunGuard_ScheduledFailedCloseRetriesWithoutRecommenting(t *testing.T) {
	t.Parallel()
	d, buf := newScheduledOverrunDaemon(t, "", false)
	f := newOverrunFake(61*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})
	f.failOn = map[string]error{"close": errors.New("bd down")}

	d.runOverrunGuard(f.seams())

	if !strings.Contains(buf.String(), "close failed (1 of 5): bd down") {
		t.Errorf("failure not logged:\n%s", buf.String())
	}
	if slices.Contains(f.calls, "retire gastown/capable") || slices.Contains(f.calls, "reset-agent-bead gastown/capable") {
		t.Errorf("seat released although the bead is still hooked: %v", f.calls)
	}
	st, _ := readOverrunState(d.config.TownRoot, "gt-r71dd")
	if !st.StoppedAt.IsZero() || st.CommentedAt.IsZero() || st.CloseFailures != 1 {
		t.Errorf("state = %+v, want a pending stop that commented and failed one close", st)
	}

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	want := []string{"recheck gastown/capable", "close gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("resume calls = %v, want %v (no second comment)", f.calls, want)
	}
	if len(f.comments) != 1 || len(f.closed) != 1 {
		t.Errorf("comments %q, closed %q; want one of each", f.comments, f.closed)
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-r71dd"); st.StoppedAt.IsZero() {
		t.Errorf("stop still pending after the release: %+v", st)
	}
}

// A close that never lands is given up on after five tries: the stop ends with the
// seat left paused and the bead hooked (an operator closes and nukes by hand), and
// the guard does not loop on it.
func TestRunOverrunGuard_ScheduledCloseGivenUpLeavesSeatPaused(t *testing.T) {
	t.Parallel()
	d, buf := newScheduledOverrunDaemon(t, "", false)
	f := newOverrunFake(61*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})
	f.failOn = map[string]error{"close": errors.New("bd down")}

	d.runOverrunGuard(f.seams())
	f.seats = nil
	for range overrunCommentMaxAttempts {
		f.now = f.now.Add(time.Minute)
		d.runOverrunGuard(f.seams())
	}

	if !strings.Contains(buf.String(), "giving up on closing the bead; the seat stays paused") {
		t.Errorf("give-up not logged:\n%s", buf.String())
	}
	st, _ := readOverrunState(d.config.TownRoot, "gt-r71dd")
	if st.StoppedAt.IsZero() || st.CloseFailures != overrunCommentMaxAttempts || !st.ClosedAt.IsZero() {
		t.Errorf("state = %+v, want a finished stop after %d failed closes and no close", st, overrunCommentMaxAttempts)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "retire") || strings.HasPrefix(c, "reset-agent-bead") {
			t.Errorf("seat released although the bead was never closed: %v", f.calls)
		}
	}
	n := len(f.calls)
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.calls) != n {
		t.Errorf("the guard acted again after giving up: %v", f.calls[n:])
	}
}

// A retire that fails leaves the release pending: the next tick repeats from the
// agent-bead reset, and neither the comment nor the close is repeated.
func TestRunOverrunGuard_ScheduledFailedRetireRepeatsOnlyTheRelease(t *testing.T) {
	t.Parallel()
	d, buf := newScheduledOverrunDaemon(t, "", false)
	f := newOverrunFake(61*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true})
	f.failOn = map[string]error{"retire": errors.New("intent locked")}

	d.runOverrunGuard(f.seams())

	if !strings.Contains(buf.String(), "seat not retired after closing gt-r71dd: intent locked") {
		t.Errorf("failure not logged:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-r71dd"); !st.StoppedAt.IsZero() || st.ClosedAt.IsZero() {
		t.Errorf("state = %+v, want a pending stop with the bead closed", st)
	}

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	want := []string{"recheck gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("resume calls = %v, want %v", f.calls, want)
	}
	if len(f.comments) != 1 || len(f.closed) != 1 {
		t.Errorf("comments %q, closed %q; want one of each, not repeated", f.comments, f.closed)
	}
}

// scheduledCloseFake is a run bead behind the production BeadWriter, with a
// force close that can be made to fail.
type scheduledCloseFake struct {
	*bdFake
	failClose bool
	closed    []string
}

func (f *scheduledCloseFake) force(reason string, ids ...string) error {
	if f.failClose {
		return errors.New("bd down")
	}
	f.closed = append(f.closed, ids...)
	f.issue.Status = "closed"
	return nil
}

// The first close cleared the assignee and failed; the retry is a tick later,
// and the dispatcher or a landing may have taken the bead since. The retry
// closes the half-closed bead, and leaves a re-slung or already-closed one
// alone.
func TestScheduledCloseSkipsReassignedBead(t *testing.T) {
	t.Parallel()
	const seat = "gastown/polecats/capable"
	for _, tc := range []struct {
		name, status, assignee string
		wantClosed             bool
	}{
		{"untouched since the failed close", "hooked", "", true},
		{"re-slung to another polecat", "hooked", "gastown/polecats/nux", false},
		{"re-slung and started", "in_progress", "gastown/polecats/nux", false},
		{"closed meanwhile", "closed", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &scheduledCloseFake{
				bdFake:    &bdFake{issue: beads.Issue{ID: "gt-r71dd", Status: "hooked", Assignee: seat}},
				failClose: true,
			}
			w := polecat.NewBeadWriter(f.bdFake, overrunActor)
			ctx := context.Background()

			if _, err := closeRunBead(ctx, w, f.force, "gt-r71dd", seat, "r"); err == nil {
				t.Fatal("first close succeeded, want the failure")
			}
			if f.issue.Assignee != "" {
				t.Fatalf("the failed close left the assignee %q, want it cleared", f.issue.Assignee)
			}

			// Between the ticks.
			f.issue.Status, f.issue.Assignee, f.failClose = tc.status, tc.assignee, false
			skipped, err := closeRunBead(ctx, w, f.force, "gt-r71dd", seat, "r")
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if got := len(f.closed) == 1; got != tc.wantClosed {
				t.Errorf("closed = %v, want closed %v", f.closed, tc.wantClosed)
			}
			if (skipped != "") == tc.wantClosed {
				t.Errorf("skipped = %q, want a reason exactly when the bead is left alone", skipped)
			}
			if !tc.wantClosed && (f.issue.Assignee != tc.assignee || f.issue.Status != tc.status) {
				t.Errorf("retry changed the bead to %q assigned to %q, want %q assigned to %q untouched",
					f.issue.Status, f.issue.Assignee, tc.status, tc.assignee)
			}
		})
	}
}

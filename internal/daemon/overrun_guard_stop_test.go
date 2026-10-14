package daemon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// stopSeat is the seat the stop tests act on.
var stopSeat = overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11", Branch: "polecat/capable/gt-aaa11+x1"}

func TestStop_OrderIsPauseStopCheckpointHandBack(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)

	d.runOverrunGuard(f.seams())

	want := []string{
		"pause gastown/capable",
		"stop gastown/capable",
		"checkpoint gastown/capable",
		"hand-back gastown/capable",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("call order = %q, want %q", f.calls, want)
	}
	if len(f.handedBack) != 1 || f.handedBack[0] != "gt-aaa11 polecat/capable/gt-aaa11+x1 20m0s" {
		t.Errorf("hand-back got %q, want the bead, its branch and 20m0s", f.handedBack)
	}
	st, err := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if err != nil {
		t.Fatal(err)
	}
	if st.StoppedAt.IsZero() {
		t.Error("StoppedAt not persisted after the hand-back")
	}
}

func TestStop_FailedSessionStopClearsMarkerAndSkipsHandBack(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(21*time.Minute, stopSeat)
	f.failOn = map[string]error{"stop": errors.New("tmux refused")}

	d.runOverrunGuard(f.seams())

	want := []string{"pause gastown/capable", "stop gastown/capable", "clear-pause gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("call order = %q, want %q (marker cleared, nothing after the failed stop)", f.calls, want)
	}
	if len(f.handedBack) != 0 {
		t.Errorf("bead handed back after a failed stop: %q", f.handedBack)
	}
	if !strings.Contains(buf.String(), "session stop failed: tmux refused") {
		t.Errorf("log lacks the stop failure:\n%s", buf.String())
	}
	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if !st.StopBeganAt.IsZero() || !st.StoppedAt.IsZero() {
		t.Errorf("state %+v still records a stop under way", st)
	}

	// The next tick starts over from the pause, never from the middle.
	f.calls, f.failOn = nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.calls) != 4 || f.calls[0] != "pause gastown/capable" || f.calls[3] != "hand-back gastown/capable" {
		t.Errorf("retry call order = %q, want the full sequence", f.calls)
	}
}

func TestStop_FailedMarkerClearKeepsTheStopPending(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(21*time.Minute, stopSeat)
	f.failOn = map[string]error{"stop": errors.New("tmux refused"), "clear-pause": errors.New("disk full")}

	d.runOverrunGuard(f.seams())
	// The seat is parked now, so the working-seat list no longer shows it; the
	// saved state is what brings the stop back.
	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	want := []string{"stop gastown/capable", "checkpoint gastown/capable", "hand-back gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("resumed call order = %q, want %q (no second pause)", f.calls, want)
	}
}

func TestStop_RunsOnceAfterStoppedAt(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)

	d.runOverrunGuard(f.seams())
	first := len(f.calls)
	if first != 4 {
		t.Fatalf("first tick made %d calls (%q), want 4", first, f.calls)
	}

	// The seat is still listed (say the session came back): the stop is done
	// for this sling and must not run again.
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.calls) != first {
		t.Errorf("next tick repeated the stop: %q", f.calls[first:])
	}

	// A restart is a new Daemon over the same town.
	d2, _ := newOverrunDaemon(t, false)
	d2.config.TownRoot = d.config.TownRoot
	d2.runOverrunGuard(f.seams())
	if len(f.calls) != first {
		t.Errorf("a restarted daemon repeated the stop: %q", f.calls[first:])
	}
}

func TestStop_ReslingAfterStopRunsAFreshStop(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	d.runOverrunGuard(f.seams())

	resling := overrunSlung.Add(40 * time.Minute)
	f.slung["gt-aaa11"] = resling
	f.calls = nil
	f.now = resling.Add(21 * time.Minute)
	d.runOverrunGuard(f.seams())

	if len(f.calls) != 4 {
		t.Errorf("calls for the new sling = %q, want a full stop", f.calls)
	}
}

func TestStop_FailedCheckpointRetriesWithoutRepeatingEarlierSteps(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("git busy")}

	d.runOverrunGuard(f.seams())
	want := []string{"pause gastown/capable", "stop gastown/capable", "checkpoint gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %q, want %q (no hand-back past a failed checkpoint)", f.calls, want)
	}
	if !strings.Contains(buf.String(), "checkpoint failed: git busy") {
		t.Errorf("log lacks the checkpoint failure:\n%s", buf.String())
	}

	// The session is gone, so the seat is no longer listed; the record
	// carries the retry.
	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	want = []string{"checkpoint gastown/capable", "hand-back gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("retry calls = %q, want %q", f.calls, want)
	}
	// The elapsed time is the one measured when the stop began.
	if len(f.handedBack) != 1 || !strings.HasSuffix(f.handedBack[0], " 22m0s") {
		t.Errorf("hand-back %q, want elapsed 22m0s from the first tick", f.handedBack)
	}
}

func TestStop_FailedHandBackRetriesAndStaysUnfinished(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"hand-back": errors.New("bd down")}

	d.runOverrunGuard(f.seams())
	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if !st.StoppedAt.IsZero() {
		t.Fatalf("StoppedAt set although the hand-back failed: %+v", st)
	}

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if want := []string{"hand-back gastown/capable"}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("retry calls = %q, want only the hand-back", f.calls)
	}
	st, _ = readOverrunState(d.config.TownRoot, "gt-aaa11")
	if st.StoppedAt.IsZero() {
		t.Error("StoppedAt not set after the retried hand-back")
	}
}

func TestStop_FailedPauseDoesNothingElse(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(25*time.Minute, stopSeat)
	f.failOn = map[string]error{"pause": errors.New("no intent dir")}

	d.runOverrunGuard(f.seams())

	if want := []string{"pause gastown/capable"}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %q, want only the failed pause", f.calls)
	}
}

func TestStop_DryRunCallsNoStopSeam(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, true)
	f := newOverrunFake(30*time.Minute, stopSeat)

	d.runOverrunGuard(f.seams())

	if len(f.calls) != 0 {
		t.Errorf("dry run called stop seams: %q", f.calls)
	}
}

func TestNudge_MessageAsksForSplitProposal(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"split proposal", "bead notes"} {
		if !strings.Contains(overrunNudgeMessage, want) {
			t.Errorf("nudge message lacks %q: %s", want, overrunNudgeMessage)
		}
	}
	if strings.Contains(strings.ToLower(overrunNudgeMessage), "flaky") {
		t.Errorf("nudge message uses the word flaky: %s", overrunNudgeMessage)
	}
}

// bdFake is an in-memory bead behind the production BeadWriter: HandBack runs
// over it for real, so a test can read the bead the roster would read.
type bdFake struct{ issue beads.Issue }

func (b *bdFake) Update(id string, opts beads.UpdateOptions) error {
	if opts.Status != nil {
		b.issue.Status = *opts.Status
	}
	if opts.Assignee != nil {
		b.issue.Assignee = *opts.Assignee
	}
	b.issue.Labels = append(b.issue.Labels, opts.AddLabels...)
	return nil
}

func (b *bdFake) AddCommentAs(id, author, comment string) error { return nil }

func TestStop_HandBackLeavesTheBeadUnhooked(t *testing.T) {
	t.Parallel()
	bd := &bdFake{issue: beads.Issue{
		ID: "gt-aaa11", Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/capable",
	}}
	w := polecat.NewBeadWriter(bd, overrunActor)

	if err := polecat.HandBack(context.Background(), w, "gt-aaa11", "polecat/capable/gt-aaa11+x1", 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	if bd.issue.Status != "open" || bd.issue.Assignee != "" {
		t.Errorf("bead is %q assigned to %q, want open and unassigned (nothing hooked)", bd.issue.Status, bd.issue.Assignee)
	}
	if !reflect.DeepEqual(bd.issue.Labels, []string{polecat.NeedsSplitLabel}) {
		t.Errorf("labels = %v, want [needs-split]", bd.issue.Labels)
	}
}

func TestCollectOverrunStopped_ItemNamesBeadAndElapsed(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	if err := writeOverrunState(d.config.TownRoot, overrunState{
		Bead: "gt-aaa11", Elapsed: 20*time.Minute + 3*time.Second, StoppedAt: overrunSlung,
	}); err != nil {
		t.Fatal(err)
	}
	d.openWorkBeads = func([]string, time.Duration) workBeadReader {
		return needsSplitReader{&beads.Issue{ID: "gt-aaa11", Status: "open", Labels: []string{polecat.NeedsSplitLabel}}}
	}
	d.knownRigsCache, d.knownRigsCacheValid = []string{"gastown"}, true
	s := &attentionSources{d: d, now: overrunSlung}

	items, err := s.collectOverrunStopped(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one", items)
	}
	it := items[0]
	if it.Kind != attention.KindOverrunStopped || it.Severity != attention.SeverityLow || it.Bead != "gt-aaa11" {
		t.Errorf("item = %+v, want a low overrun-stopped item on gt-aaa11", it)
	}
	if !strings.Contains(it.Summary, "gt-aaa11") || !strings.Contains(it.Summary, "20m3s") {
		t.Errorf("summary %q lacks the bead or the elapsed time", it.Summary)
	}
}

// needsSplitReader answers every list with its one issue.
type needsSplitReader struct{ issue *beads.Issue }

func (r needsSplitReader) Show(string) (*beads.Issue, error) { return r.issue, nil }
func (r needsSplitReader) List(beads.ListOptions) ([]*beads.Issue, error) {
	return []*beads.Issue{r.issue}, nil
}

// A seat past the limit whose gt done is running (the re-check says submitted)
// is not paused, stopped or handed back, and nothing is recorded, so the guard
// judges it again next tick.
func TestStop_SeatInGtDoneIsNotStopped(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(25*time.Minute, stopSeat)
	f.submitted = true

	d.runOverrunGuard(f.seams())

	if len(f.calls) != 0 {
		t.Errorf("a seat in gt done had stop steps run: %q", f.calls)
	}
	if !strings.Contains(buf.String(), "submitted before the stop") {
		t.Errorf("log lacks the leaving-it line:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); !st.StopBeganAt.IsZero() || !st.StoppedAt.IsZero() {
		t.Errorf("state %+v recorded a stop that did not happen", st)
	}
}

// A bead that gains gt:ready-to-land between the seat listing and the
// hand-back is not handed back: it would carry needs-split and ready-to-land
// at once. The pause is released and the stop is recorded finished.
func TestStop_BeadReadyToLandMidStopIsNotHandedBack(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(21*time.Minute, stopSeat)
	f.readyAfterStop = true

	d.runOverrunGuard(f.seams())

	want := []string{
		"pause gastown/capable",
		"stop gastown/capable",
		"checkpoint gastown/capable",
		"clear-pause gastown/capable",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("call order = %q, want %q (no hand-back)", f.calls, want)
	}
	if len(f.handedBack) != 0 {
		t.Errorf("submitted bead handed back: %q", f.handedBack)
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.StoppedAt.IsZero() {
		t.Error("StoppedAt not saved; the next tick would repeat the stop")
	}
}

// With no gt done signal the seat is stopped and handed back as before.
func TestStop_SeatWithNoGtDoneSignalIsStillStopped(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(21*time.Minute, stopSeat)

	d.runOverrunGuard(f.seams())

	if len(f.stops) != 1 || len(f.handedBack) != 1 {
		t.Errorf("stops %v, hand-backs %v; want one of each", f.stops, f.handedBack)
	}
}

func TestHeartbeatShowsGtDone(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	hb := func(state polecat.HeartbeatState, age time.Duration) *polecat.SessionHeartbeat {
		return &polecat.SessionHeartbeat{Timestamp: now.Add(-age), State: state}
	}
	tests := []struct {
		name string
		hb   *polecat.SessionHeartbeat
		want bool
	}{
		{"no heartbeat", nil, false},
		{"fresh exiting", hb(polecat.HeartbeatExiting, 30*time.Second), true},
		{"exiting just inside the stale window", hb(polecat.HeartbeatExiting, polecat.SessionHeartbeatStaleThreshold-time.Second), true},
		{"stale exiting is a dead gt done", hb(polecat.HeartbeatExiting, polecat.SessionHeartbeatStaleThreshold), false},
		{"fresh working", hb(polecat.HeartbeatWorking, time.Second), false},
		{"fresh idle", hb(polecat.HeartbeatIdle, time.Second), false},
		{"v1 heartbeat without state", hb("", time.Second), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := heartbeatShowsGtDone(tt.hb, now); got != tt.want {
				t.Errorf("heartbeatShowsGtDone = %v, want %v", got, tt.want)
			}
		})
	}
}

// gtDoneInFlight reads the heartbeat gt done writes through the polecat package.
func TestGtDoneInFlight_ReadsTheHeartbeatFile(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	const sess = "gt-capable"
	if d.gtDoneInFlight(sess) {
		t.Error("in flight with no heartbeat file")
	}
	polecat.TouchSessionHeartbeatWithState(d.config.TownRoot, sess, polecat.HeartbeatWorking, "work", "gt-aaa11")
	if d.gtDoneInFlight(sess) {
		t.Error("a working heartbeat read as gt done")
	}
	polecat.TouchSessionHeartbeatWithState(d.config.TownRoot, sess, polecat.HeartbeatExiting, "gt done", "gt-aaa11")
	if !d.gtDoneInFlight(sess) {
		t.Error("a fresh exiting heartbeat not read as gt done")
	}
}

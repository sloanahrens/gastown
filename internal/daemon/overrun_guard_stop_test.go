package daemon

import (
	"context"
	"errors"
	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"os"
	"path/filepath"
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
		"clear-pause gastown/capable",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("call order = %q, want %q (the seat is released after the hand-back)", f.calls, want)
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

// A pause that cannot be cleared after the hand-back leaves the seat parked,
// so the stop stays pending: the next tick repeats the hand-back and the clear
// instead of finishing with the park unannounced.
func TestStop_FailedReleaseAfterHandBackKeepsTheStopPending(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"clear-pause": errors.New("disk full")}

	d.runOverrunGuard(f.seams())

	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if !st.StoppedAt.IsZero() {
		t.Error("StoppedAt written although the seat is still parked")
	}
	if !strings.Contains(buf.String(), "pause marker not cleared after the hand-back") {
		t.Errorf("log lacks the failed release:\n%s", buf.String())
	}

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	want := []string{"recheck gastown/capable", "hand-back gastown/capable", "clear-pause gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("resumed call order = %q, want %q", f.calls, want)
	}
	st, _ = readOverrunState(d.config.TownRoot, "gt-aaa11")
	if st.StoppedAt.IsZero() {
		t.Error("StoppedAt not persisted once the seat was released")
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
	if len(f.calls) != 5 || f.calls[0] != "pause gastown/capable" || f.calls[3] != "hand-back gastown/capable" || f.calls[4] != "clear-pause gastown/capable" {
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

	want := []string{"stop gastown/capable", "checkpoint gastown/capable", "hand-back gastown/capable", "clear-pause gastown/capable"}
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
	if first != 5 {
		t.Fatalf("first tick made %d calls (%q), want 5", first, f.calls)
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

	if len(f.calls) != 5 {
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
	if !strings.Contains(buf.String(), "checkpoint failed (1 of 5): git busy") {
		t.Errorf("log lacks the checkpoint failure:\n%s", buf.String())
	}

	// The session is gone, so the seat is no longer listed; the record
	// carries the retry.
	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	want = []string{"recheck gastown/capable", "checkpoint gastown/capable", "hand-back gastown/capable", "clear-pause gastown/capable"}
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
	if want := []string{"recheck gastown/capable", "hand-back gastown/capable", "clear-pause gastown/capable"}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("retry calls = %q, want the hand-back and the release", f.calls)
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
type bdFake struct {
	issue beads.Issue
	// failLabel makes the needs-split label write fail, the partial hand-back.
	failLabel bool
}

func (b *bdFake) Show(id string) (*beads.Issue, error) {
	is := b.issue
	return &is, nil
}

func (b *bdFake) Update(id string, opts beads.UpdateOptions) error {
	if len(opts.AddLabels) > 0 && b.failLabel {
		return errors.New("bd down")
	}
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

	if err := polecat.HandBack(context.Background(), w, "gt-aaa11", "gastown/polecats/capable", "polecat/capable/gt-aaa11+x1", 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	if bd.issue.Status != "open" || bd.issue.Assignee != "" {
		t.Errorf("bead is %q assigned to %q, want open and unassigned (nothing hooked)", bd.issue.Status, bd.issue.Assignee)
	}
	if !reflect.DeepEqual(bd.issue.Labels, []string{polecat.NeedsSplitLabel}) {
		t.Errorf("labels = %v, want [needs-split]", bd.issue.Labels)
	}
}

// The first hand-back opened and unassigned the bead but failed at the label;
// the dispatcher slung it on, or it was closed. The retry must not reopen it or
// clear the new polecat's assignee.
func TestStop_HandBackRetryLeavesAReSlungOrClosedBeadAlone(t *testing.T) {
	t.Parallel()
	const stopped = "gastown/polecats/capable"
	for _, tc := range []struct {
		name, status, assignee string
	}{
		{"re-slung to another polecat", string(beads.IssueStatusHooked), "gastown/polecats/nux"},
		{"re-slung and started", "in_progress", "gastown/polecats/nux"},
		{"closed", "closed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := &bdFake{
				issue:     beads.Issue{ID: "gt-aaa11", Status: string(beads.IssueStatusHooked), Assignee: stopped},
				failLabel: true,
			}
			w := polecat.NewBeadWriter(bd, overrunActor)
			err := polecat.HandBack(context.Background(), w, "gt-aaa11", stopped, "b", 20*time.Minute)
			if err == nil || errors.Is(err, polecat.ErrHandBackSkipped) {
				t.Fatalf("first hand-back err = %v, want the label failure", err)
			}
			if bd.issue.Status != "open" || bd.issue.Assignee != "" {
				t.Fatalf("partial hand-back left %q assigned to %q, want open and unassigned", bd.issue.Status, bd.issue.Assignee)
			}

			// Between the ticks: the dispatcher re-slings it, or it lands.
			bd.issue.Status, bd.issue.Assignee, bd.failLabel = tc.status, tc.assignee, false
			err = polecat.HandBack(context.Background(), w, "gt-aaa11", stopped, "b", 20*time.Minute)
			if !errors.Is(err, polecat.ErrHandBackSkipped) {
				t.Errorf("retry err = %v, want ErrHandBackSkipped", err)
			}
			if bd.issue.Status != tc.status || bd.issue.Assignee != tc.assignee || len(bd.issue.Labels) != 0 {
				t.Errorf("retry changed the bead to %q assigned to %q labels %v, want %q assigned to %q untouched",
					bd.issue.Status, bd.issue.Assignee, bd.issue.Labels, tc.status, tc.assignee)
			}
		})
	}
}

// With nobody else on the bead the retry finishes the half-released hand-back.
func TestStop_HandBackRetryFinishesAnUntouchedPartialHandBack(t *testing.T) {
	t.Parallel()
	const stopped = "gastown/polecats/capable"
	bd := &bdFake{
		issue:     beads.Issue{ID: "gt-aaa11", Status: string(beads.IssueStatusHooked), Assignee: stopped},
		failLabel: true,
	}
	w := polecat.NewBeadWriter(bd, overrunActor)
	if err := polecat.HandBack(context.Background(), w, "gt-aaa11", stopped, "b", 20*time.Minute); err == nil {
		t.Fatal("first hand-back succeeded, want the label failure")
	}
	bd.failLabel = false
	if err := polecat.HandBack(context.Background(), w, "gt-aaa11", stopped, "b", 20*time.Minute); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if bd.issue.Status != "open" || bd.issue.Assignee != "" || !reflect.DeepEqual(bd.issue.Labels, []string{polecat.NeedsSplitLabel}) {
		t.Errorf("bead = %q assigned to %q labels %v, want open, unassigned, [needs-split]", bd.issue.Status, bd.issue.Assignee, bd.issue.Labels)
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

// A checkpoint the worktree refuses (mid-rebase, a failed git add) fails step 3:
// the hand-back comment names the branch as holding the work, so no hand-back
// runs until a retry commits it (gt-0frud).
func TestStop_RefusedCheckpointKeepsTheStopPendingAndIsRetried(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("checkpoint refused; uncommitted work may remain in the worktree")}

	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 0 {
		t.Fatalf("handed back past a refused checkpoint: %q", f.handedBack)
	}
	if !strings.Contains(buf.String(), "checkpoint failed (1 of 5): checkpoint refused") {
		t.Errorf("log lacks the refusal:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); !st.CheckpointedAt.IsZero() || !st.StoppedAt.IsZero() {
		t.Errorf("state %+v counts a refused checkpoint as done", st)
	}

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs after the retried checkpoint = %q, want one", f.handedBack)
	}
}

// A checkpoint refused on every tick is bounded (gt-98rlt): after
// overrunCheckpointMaxAttempts the bead is handed back with a note, the pause
// stays down so the worktree is not reused, and the stop does not run again.
func TestStop_PersistentlyRefusedCheckpointIsGivenUpOnAndHandedBack(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("checkpoint refused; uncommitted work may remain in the worktree")}

	for i := 1; i < overrunCheckpointMaxAttempts; i++ {
		d.runOverrunGuard(f.seams())
		if len(f.handedBack) != 0 {
			t.Fatalf("tick %d: handed back before the cap: %q", i, f.handedBack)
		}
		f.seats = nil
		f.now = f.now.Add(time.Minute)
	}
	f.calls = nil
	d.runOverrunGuard(f.seams())

	want := []string{"recheck gastown/capable", "checkpoint gastown/capable", "hand-back gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls at the cap = %q, want %q (no clear-pause: the seat stays parked)", f.calls, want)
	}
	if len(f.handedBack) != 1 || len(f.handBackNotes) != 1 {
		t.Fatalf("hand-backs = %q notes = %q, want one each", f.handedBack, f.handBackNotes)
	}
	for _, w := range []string{"could not be checkpointed", "5 attempts", "gastown/capable stays paused", "gt agent resume"} {
		if !strings.Contains(f.handBackNotes[0], w) {
			t.Errorf("hand-back note %q lacks %q", f.handBackNotes[0], w)
		}
	}
	if !strings.Contains(buf.String(), "giving up on the checkpoint") {
		t.Errorf("log lacks the give-up line:\n%s", buf.String())
	}
	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if st.StoppedAt.IsZero() || !st.CheckpointedAt.IsZero() || !st.CheckpointGaveUp {
		t.Errorf("state %+v, want stopped, not checkpointed, gave up", st)
	}

	f.calls = nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.calls) != 0 || len(f.handedBack) != 1 {
		t.Errorf("a finished stop ran again: calls %q hand-backs %q", f.calls, f.handedBack)
	}
}

// A seat taken back before the cap is never handed back, however many
// checkpoints had failed: the give-up path sits behind the same re-check.
func TestStop_TakenBackSeatIsNotHandedBackAtTheCap(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("git busy")}
	for i := 1; i < overrunCheckpointMaxAttempts; i++ {
		d.runOverrunGuard(f.seams())
		f.seats = nil
		f.now = f.now.Add(time.Minute)
	}
	f.lapsed = "a session is running on the seat"
	f.calls = nil
	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 0 || !reflect.DeepEqual(f.calls, []string{"recheck gastown/capable"}) {
		t.Errorf("calls %q hand-backs %q, want only the re-check", f.calls, f.handedBack)
	}
}

// Failures short of the cap that end in a good checkpoint hand the bead back
// with no note and release the seat as before.
func TestStop_CheckpointThatRecoversHandsBackWithoutANote(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("git busy")}
	d.runOverrunGuard(f.seams())
	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.handBackNotes) != 1 || f.handBackNotes[0] != "" {
		t.Errorf("notes = %q, want one empty note", f.handBackNotes)
	}
	if got := f.calls[len(f.calls)-1]; got != "clear-pause gastown/capable" {
		t.Errorf("last call %q, want the pause cleared", got)
	}
}

// A resumed stop whose seat was taken back is abandoned: the bead belongs to
// whoever holds it now (gt-0frud).
func TestStop_ResumedStopOnATakenBackSeatIsAbandoned(t *testing.T) {
	t.Parallel()
	for _, why := range []string{"the pause marker is gone", "a session is running on the seat"} {
		t.Run(why, func(t *testing.T) {
			d, buf := newOverrunDaemon(t, false)
			f := newOverrunFake(22*time.Minute, stopSeat)
			f.failOn = map[string]error{"checkpoint": errors.New("git busy")}
			d.runOverrunGuard(f.seams())

			f.seats, f.calls, f.failOn = nil, nil, nil
			f.lapsed = why
			f.now = f.now.Add(time.Minute)
			d.runOverrunGuard(f.seams())

			if want := []string{"recheck gastown/capable"}; !reflect.DeepEqual(f.calls, want) {
				t.Errorf("calls = %q, want only the re-check (no checkpoint, hand-back or pause change)", f.calls)
			}
			if len(f.handedBack) != 0 {
				t.Errorf("handed back a bead whose seat was taken back: %q", f.handedBack)
			}
			if !strings.Contains(buf.String(), "abandoning the stop") || !strings.Contains(buf.String(), why) {
				t.Errorf("log lacks the abandon line naming %q:\n%s", why, buf.String())
			}
			if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.StoppedAt.IsZero() {
				t.Error("StoppedAt not saved; the next tick would resume the abandoned stop")
			}

			f.calls = nil
			f.lapsed = ""
			f.now = f.now.Add(time.Minute)
			d.runOverrunGuard(f.seams())
			if len(f.calls) != 0 {
				t.Errorf("an abandoned stop ran again: %q", f.calls)
			}
		})
	}
}

// A re-check that cannot read the seat keeps the stop pending.
func TestStop_ResumedStopRetriesWhenTheSeatCannotBeRead(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("git busy")}
	d.runOverrunGuard(f.seams())

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.lapsedErr = errors.New("tmux down")
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if want := []string{"recheck gastown/capable"}; !reflect.DeepEqual(f.calls, want) || len(f.handedBack) != 0 {
		t.Fatalf("calls = %q handed back %q, want the re-check only", f.calls, f.handedBack)
	}

	f.calls, f.lapsedErr = nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs once the seat could be read = %q, want one", f.handedBack)
	}
}

// A stop that is not resumed does not re-check: it has just stopped the session.
func TestStop_FirstRunDoesNotRecheckTheSeat(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.lapsed = "a session is running on the seat"
	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 1 {
		t.Errorf("a first-run stop was abandoned: calls %q", f.calls)
	}
}

// checkpointSeat builds a polecat worktree under the daemon's town and returns
// the checkout.
func checkpointSeat(t *testing.T, d *Daemon, files map[string]string) string {
	t.Helper()
	f := useGitfake(t, d)
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	workDir := filepath.Join(d.config.TownRoot, stopSeat.Rig, "polecats", stopSeat.Polecat, stopSeat.Rig)
	f.InitBare(t, origin)
	f.Commit(t, origin, "main", "initial", files)
	f.Clone(t, origin, workDir)
	return workDir
}

// The production checkpoint seam fails a refused checkpoint and passes a
// created one and a clean worktree.
func TestOverrunCheckpointSeam_FailsOnlyARefusedCheckpoint(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	workDir := checkpointSeat(t, d, map[string]string{"a.go": "package a\n"})
	seam := d.overrunStopSeams(overrunSeams{}).checkpoint

	if err := seam(stopSeat); err != nil {
		t.Errorf("clean worktree: %v", err)
	}
	writeWorkFiles(t, workDir, map[string]string{"a.go": "package a\n// work\n"})
	if err := seam(stopSeat); err != nil {
		t.Errorf("created checkpoint: %v", err)
	}
	writeWorkFiles(t, workDir, map[string]string{"a.go": "package a\n// more work\n"})
	if err := os.WriteFile(filepath.Join(workDir, ".git", "MERGE_HEAD"), []byte(strings.Repeat("0", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := seam(stopSeat); err == nil {
		t.Error("a worktree mid-merge must fail the checkpoint step")
	}
}

// The production re-check reads the pause marker and then the session.
func TestOverrunStopLapsed_ReadsThePauseMarkerAndTheSession(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	tm := newFakeTmux(newFixedClock())
	d.tmux = tm

	if why, err := d.overrunStopLapsed(stopSeat); err != nil || why != "the pause marker is gone" {
		t.Errorf("no marker: got %q, %v", why, err)
	}

	if err := agentpause.Pause(d.config.TownRoot, stopSeat.Rig, constants.RolePolecat, stopSeat.Polecat, overrunPauseReason, overrunActor, ""); err != nil {
		t.Fatal(err)
	}
	if why, err := d.overrunStopLapsed(stopSeat); err != nil || why != "" {
		t.Errorf("paused, no session: got %q, %v; want the stop to still own the seat", why, err)
	}

	name := session.PolecatSessionName(d.prefixRegistry().PrefixForRig(stopSeat.Rig), stopSeat.Polecat)
	tm.addSession(name, "node", time.Now())
	if why, err := d.overrunStopLapsed(stopSeat); err != nil || why != "a session is running on the seat" {
		t.Errorf("paused with a live session: got %q, %v", why, err)
	}
}

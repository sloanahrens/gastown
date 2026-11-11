package daemon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// divergedSandboxError is the refusal a sandbox push returns when its branch
// diverged from origin, with both heads named.
func divergedSandboxError() error {
	return &session.BranchDivergedError{
		Branch: stopSeat.Branch, Local: "aaa111", Remote: "bbb222", MergeBase: "ccc333",
		LocalOnly: 2, RemoteOnly: 1, Sandbox: overrunStopSessionName,
		Refusal: errors.New("! [rejected] (non-fast-forward)"),
	}
}

// A sandbox whose push origin refuses as diverged is kept on every tick, even
// when a later read reports it gone: no worktree reset, no sandbox delete, and
// the bead is handed back once with a note naming the sandbox and both heads.
// The sandbox is asked once, by its refused push.
func TestStop_DivergedSandboxIsKeptAndNotAskedAgainOverTicks(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	seedStopSandbox(t, d)
	f := newOverrunFake(20*time.Minute, stopSeat)

	pushes := 0
	var deleted []string
	f.removeSandbox = func(seat overrunSeat) error {
		return d.overrunRemoveSandbox(seat, func(string, session.SandboxRecord) error {
			pushes++
			if pushes == 1 {
				return divergedSandboxError()
			}
			// The sandbox reads gone on a later tick: its clone is not found.
			return session.ErrNothingToPush
		}, func(_ string, rec session.SandboxRecord) error {
			deleted = append(deleted, rec.Session)
			return nil
		})
	}
	f.resetWorktree = func(overrunSeat) error { return nil }

	d.runOverrunGuard(f.seams())
	want := []string{"pause gastown/capable", "stop gastown/capable", "sandbox gastown/capable", "checkpoint gastown/capable", "push gastown/capable", "hand-back gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("first tick calls = %q, want %q (the refusal ends the stop with the sandbox kept, no reset, no release)", f.calls, want)
	}
	for i := 0; i < 5; i++ {
		pushTick(d, f)
		if len(f.calls) != 0 {
			t.Errorf("tick %d after the hand-back ran %q, want nothing", i+2, f.calls)
		}
	}

	if pushes != 1 {
		t.Errorf("sandbox asked %d times, want the one refused push only", pushes)
	}
	if len(deleted) != 0 {
		t.Errorf("sandbox deleted despite the diverged push: %q", deleted)
	}
	if len(f.handedBack) != 1 || len(f.handBackNotes) != 1 {
		t.Fatalf("hand-backs = %q notes = %q, want one each", f.handedBack, f.handBackNotes)
	}
	for _, w := range []string{overrunStopSessionName, "aaa111", "bbb222", "gastown/capable stays paused", "gt agent resume"} {
		if !strings.Contains(f.handBackNotes[0], w) {
			t.Errorf("hand-back note %q lacks %q", f.handBackNotes[0], w)
		}
	}
	if _, ok, err := session.ReadSandboxRecord(d.config.TownRoot, overrunStopSessionName); err != nil || !ok {
		t.Errorf("the sandbox record was removed despite the diverged push (read ok %v, err %v)", ok, err)
	}

	// The hand-back put needs-split on the bead: the collector raises one
	// overrun item for it, however many ticks the seat stayed held.
	d.openWorkBeads = func([]string, time.Duration) workBeadReader {
		return needsSplitReader{&beads.Issue{ID: stopSeat.Bead, Status: "open", Labels: []string{polecat.NeedsSplitLabel}}}
	}
	d.knownRigsCache, d.knownRigsCacheValid = []string{stopSeat.Rig}, true
	s := &attentionSources{d: d, now: overrunSlung}
	items, err := s.collectOverrunStopped(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Bead != stopSeat.Bead {
		t.Errorf("attention items = %+v, want exactly one for %s", items, stopSeat.Bead)
	}
}

// When the hand-back fails after a diverged refusal, the stop stays open and
// the next tick must not ask the sandbox again: it is recorded as kept, so a
// read that now finds it gone deletes nothing, and the worktree is not reset.
func TestStop_DivergedSandboxStaysKeptWhenTheHandBackFails(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	seedStopSandbox(t, d)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"hand-back": errors.New("bd unreachable")}

	pushes := 0
	var deleted []string
	f.removeSandbox = func(seat overrunSeat) error {
		return d.overrunRemoveSandbox(seat, func(string, session.SandboxRecord) error {
			pushes++
			if pushes == 1 {
				return divergedSandboxError()
			}
			return session.ErrNothingToPush
		}, func(_ string, rec session.SandboxRecord) error {
			deleted = append(deleted, rec.Session)
			return nil
		})
	}
	f.resetWorktree = func(overrunSeat) error { return nil }

	d.runOverrunGuard(f.seams())
	f.failOn = nil
	pushTick(d, f)

	if pushes != 1 || len(deleted) != 0 {
		t.Errorf("sandbox asked %d times and deleted %q after the diverged refusal, want one ask and no delete", pushes, deleted)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "reset-worktree") || strings.HasPrefix(c, "sandbox") {
			t.Errorf("later tick ran %q; the kept sandbox must not be asked or the worktree reset", c)
		}
	}
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %q, want the repeated hand-back once", f.handedBack)
	}
}

// A diverged sandbox is kept for the stop that found it. Once that stop has
// handed the bead back, the bead's next sling starts clean: the next stop asks
// the sandbox again, and a sandbox that now pushes is removed with no note
// carried over from the diverged stop.
func TestStop_DivergedSandboxIsAskedAgainByTheNextStop(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	seedStopSandbox(t, d)
	f := newOverrunFake(20*time.Minute, stopSeat)

	pushes := 0
	var deleted []string
	f.removeSandbox = func(seat overrunSeat) error {
		return d.overrunRemoveSandbox(seat, func(string, session.SandboxRecord) error {
			pushes++
			if pushes == 1 {
				return divergedSandboxError()
			}
			return session.ErrNothingToPush
		}, func(_ string, rec session.SandboxRecord) error {
			deleted = append(deleted, rec.Session)
			return nil
		})
	}
	f.resetWorktree = func(overrunSeat) error { return nil }

	d.runOverrunGuard(f.seams())
	if pushes != 1 || len(f.handBackNotes) != 1 {
		t.Fatalf("first stop: sandbox asked %d times, hand-back notes %q; want one ask and one hand-back", pushes, f.handBackNotes)
	}

	// The operator reconciles the heads and resumes the seat; the bead is
	// slung again and its new run overruns.
	resling := f.now.Add(time.Hour)
	f.slung[stopSeat.Bead] = resling
	f.now = resling.Add(20 * time.Minute)
	f.seats, f.calls = []overrunSeat{stopSeat}, nil
	d.runOverrunGuard(f.seams())

	if pushes != 2 {
		t.Errorf("sandbox asked %d times after the re-sling, want the new stop to ask it again", pushes)
	}
	if !reflect.DeepEqual(deleted, []string{overrunStopSessionName}) {
		t.Errorf("sandbox deleted = %q, want the reconciled sandbox removed by the new stop", deleted)
	}
	if len(f.handBackNotes) != 2 {
		t.Fatalf("hand-back notes = %q, want one more for the new stop", f.handBackNotes)
	}
	if strings.Contains(f.handBackNotes[1], "diverged") {
		t.Errorf("the new stop's hand-back carries the old diverged note: %q", f.handBackNotes[1])
	}
}

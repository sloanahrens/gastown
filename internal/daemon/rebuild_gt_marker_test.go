package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/installpending"
	"github.com/steveyegge/gastown/internal/version"
)

// pendingMarker returns the pending-install marker standing for the town, nil
// when none does.
func pendingMarker(t *testing.T, d *Daemon) *installpending.Marker {
	t.Helper()
	m, err := d.rebuildGTPendingStore().Pending(d.clk().Now())
	if err != nil {
		t.Fatalf("reading the pending-install marker: %v", err)
	}
	return m
}

// rebuildGTMarkerRun runs one due, quiet cycle whose install-gt answers reply,
// and returns the marker that stood while install-gt ran and the marker that
// stands after the cycle.
func rebuildGTMarkerRun(t *testing.T, reply cliReply) (during, after *installpending.Marker, settled bool) {
	t.Helper()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case c.name == "bash":
			// install-gt is running now: this is the window a reader outside
			// the job must see the install in.
			during = pendingMarker(t, d)
			return reply
		}
		return cliReply{}
	})
	settled = d.runRebuildGT()
	return during, pendingMarker(t, d), settled
}

// TestRebuildGT_WritesPendingMarkerWhileInstalling pins the marker's write
// side: a due install is recorded before install-gt runs, naming the daemon
// that started it, so a reader can tell an install is in flight (gt-vc34c).
func TestRebuildGT_WritesPendingMarkerWhileInstalling(t *testing.T) {
	t.Parallel()
	during, _, settled := rebuildGTMarkerRun(t, cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"})
	if !settled {
		t.Fatal("a completed install reached a verdict")
	}
	if during == nil {
		t.Fatal("no pending-install marker stood while install-gt ran")
	}
	if during.PID != os.Getpid() {
		t.Errorf("marker pid = %d, want the writing daemon's %d", during.PID, os.Getpid())
	}
	if during.At.IsZero() {
		t.Error("the marker names no time, so a reader cannot age it")
	}
}

// TestRebuildGT_ClearsPendingMarkerWhenTheInstallLands pins that a landed
// install leaves nothing for a reader to wait on (gt-vc34c).
func TestRebuildGT_ClearsPendingMarkerWhenTheInstallLands(t *testing.T) {
	t.Parallel()
	during, after, settled := rebuildGTMarkerRun(t, cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"})
	if !settled {
		t.Fatal("a completed install reached a verdict")
	}
	if during == nil {
		t.Fatal("no marker stood before the install, so the clear proves nothing")
	}
	if after != nil {
		t.Fatalf("the marker still stands after the install landed: %+v", after)
	}
}

// TestRebuildGT_ClearsPendingMarkerWhenTheInstallFails pins the failure side:
// an install that failed installs nothing, so the marker must not keep telling
// readers to stay out of its way (gt-vc34c).
func TestRebuildGT_ClearsPendingMarkerWhenTheInstallFails(t *testing.T) {
	t.Parallel()
	during, after, settled := rebuildGTMarkerRun(t, cliReply{stdout: "install-gt: RESULT failed - - build-failed\n", code: 1})
	if !settled {
		t.Fatal("a failed install reached a verdict")
	}
	if during == nil {
		t.Fatal("no marker stood before the install, so the clear proves nothing")
	}
	if after != nil {
		t.Fatalf("the marker still stands after the install failed: %+v", after)
	}
}

// TestRebuildGT_KeepsPendingMarkerWhenTheCycleDefers pins the other direction
// of the same rule: a cycle that accomplished nothing did not install, so the
// marker keeps standing and the next heartbeat retries over it (gt-vc34c).
func TestRebuildGT_KeepsPendingMarkerWhenTheCycleDefers(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "a gate suite holds a slot (gastown/gate)", true }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "branch") {
			return cliReply{stdout: "main\n"}
		}
		return cliReply{}
	})

	if settled := d.runRebuildGT(); settled {
		t.Fatal("a cycle that yielded to a busy gate accomplished nothing")
	}
	if installs := installCalls(cli); len(installs) != 0 {
		t.Fatalf("installed beside a running gate suite: %v", installs)
	}
	if m := pendingMarker(t, d); m == nil {
		t.Fatal("the deferral cleared the marker, so the next heartbeat has no install to retry")
	}
}

// TestRebuildGT_UnwritableMarkerDoesNotStopTheInstall pins that the marker is
// a record beside the install, not a precondition of it: a marker path that
// can be neither written nor cleared is logged and the install runs (gt-vc34c).
func TestRebuildGT_UnwritableMarkerDoesNotStopTheInstall(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case c.name == "bash":
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})
	blockPendingMarkerPath(t, d)

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("an install beside an unusable marker path reached its verdict")
	}
	if installs := installCalls(cli); len(installs) != 1 {
		t.Fatalf("installs = %v, want the one the marker path must not block", installs)
	}
}

// TestRebuildGT_EscalatesAMarkerItCannotWrite pins the other half of the same
// failure: a reader that finds no marker reads a town with no install coming,
// so the failure has to reach an operator rather than a log line alone
// (gt-vc34c).
func TestRebuildGT_EscalatesAMarkerItCannotWrite(t *testing.T) {
	t.Parallel()
	d, rec := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case c.name == "bash":
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})
	blockPendingMarkerPath(t, d)

	d.runRebuildGT()

	if open := rec.OpenEscalations(alertKeyRebuildGTMarkerWrite); len(open) != 1 {
		t.Fatalf("open escalations = %+v, want one under %s", open, alertKeyRebuildGTMarkerWrite)
	}
}

// TestRebuildGT_ClearsTheMarkerWriteAlertOnceTheMarkerIsWritten pins the close:
// the escalation holds while the marker cannot be written, and a cycle that
// writes one takes it down, so a repaired town is not left with the alarm.
func TestRebuildGT_ClearsTheMarkerWriteAlertOnceTheMarkerIsWritten(t *testing.T) {
	t.Parallel()
	d, rec := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case c.name == "bash":
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})
	blockPendingMarkerPath(t, d)
	d.runRebuildGT()
	if open := rec.OpenEscalations(alertKeyRebuildGTMarkerWrite); len(open) != 1 {
		t.Fatalf("the blocked cycle left %d escalations open, want one", len(open))
	}

	if err := os.RemoveAll(d.rebuildGTPendingStore().Path()); err != nil {
		t.Fatal(err)
	}
	if settled := d.runRebuildGT(); !settled {
		t.Fatal("the second cycle reached its verdict")
	}

	if open := rec.OpenEscalations(alertKeyRebuildGTMarkerWrite); len(open) != 0 {
		t.Fatalf("the marker was written and the escalation still stands: %+v", open)
	}
}

// TestRebuildGT_RefreshesTheMarkerAcrossTheGateWait pins that the marker moves
// with the cycle's own clock: the reserve wait can hold for minutes, and a
// marker left at the pre-wait time ages out from under an install that is still
// running (gt-vc34c).
func TestRebuildGT_RefreshesTheMarkerAcrossTheGateWait(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	clk := clockwork.NewFakeClockAt(testEpoch)
	d.clock = clk
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	// Busy for the decision, then free once the wait has begun — and the clock
	// moves inside that read, so the wait is a real interval rather than a
	// instant the test cannot tell from the pre-wait write.
	gates := 0
	d.rebuildGTGateFn = func() (string, bool) {
		gates++
		if gates == 1 {
			return "a gate suite holds a slot (gastown/gate)", true
		}
		clk.Advance(rebuildGTReserveWait / 2)
		return "", false
	}
	var during *installpending.Marker
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case c.name == "bash":
			during = pendingMarker(t, d)
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})
	d.rebuildGTBlock.open(testEpoch.Add(-31 * time.Minute))

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("a completed install holds the interval")
	}
	if during == nil {
		t.Fatal("no marker stood while install-gt ran")
	}
	if !during.At.After(testEpoch) {
		t.Fatalf("marker at = %s, want the refresh after the wait moved it past %s", during.At, testEpoch)
	}
}

// TestRebuildGT_EscalatesAMarkerItCannotClear pins that a marker which cannot
// be removed reaches an operator: it keeps telling every later reader that an
// install is in flight after this one finished, and a log line alone leaves
// that reader waiting on debris nobody is looking for (gt-vc34c).
func TestRebuildGT_EscalatesAMarkerItCannotClear(t *testing.T) {
	t.Parallel()
	d, rec := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case c.name == "bash":
			// The marker stood before install-gt ran; replace it with debris
			// that Clear cannot remove.
			replacePendingMarkerWithADirectory(t, d)
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("the cycle reached its verdict")
	}
	if open := rec.OpenEscalations(alertKeyRebuildGTMarkerClear); len(open) != 1 {
		t.Fatalf("open escalations = %+v, want one under %s", open, alertKeyRebuildGTMarkerClear)
	}
}

// TestRebuildGT_ClearsTheMarkerClearAlertOnceTheMarkerIsGone pins the close of
// that alarm: the escalation holds while the marker cannot be removed, and a
// cycle whose clear succeeds takes it down, so a repaired town keeps no
// standing alarm (gt-vc34c).
func TestRebuildGT_ClearsTheMarkerClearAlertOnceTheMarkerIsGone(t *testing.T) {
	t.Parallel()
	d, rec := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	cycles := 0
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case c.name == "bash":
			if cycles == 0 {
				replacePendingMarkerWithADirectory(t, d)
			}
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})
	run := func() {
		if settled := d.runRebuildGT(); !settled {
			t.Fatal("the cycle reached its verdict")
		}
		cycles++
	}

	run()
	if open := rec.OpenEscalations(alertKeyRebuildGTMarkerClear); len(open) != 1 {
		t.Fatalf("the failed clear left %d escalations open, want one", len(open))
	}

	if err := os.RemoveAll(d.rebuildGTPendingStore().Path()); err != nil {
		t.Fatal(err)
	}
	run()

	if open := rec.OpenEscalations(alertKeyRebuildGTMarkerClear); len(open) != 0 {
		t.Fatalf("the marker was cleared and the escalation still stands: %+v", open)
	}
}

// replacePendingMarkerWithADirectory turns the standing marker into a
// non-empty directory, which Clear cannot remove, so the clear's failure branch
// runs with a marker that was successfully written.
func replacePendingMarkerWithADirectory(t *testing.T, d *Daemon) {
	t.Helper()
	path := d.rebuildGTPendingStore().Path()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	blockPendingMarkerPath(t, d)
}

// blockPendingMarkerPath puts a non-empty directory where the marker belongs, so
// the write cannot rename over it and the clear cannot remove it.
func blockPendingMarkerPath(t *testing.T, d *Daemon) {
	t.Helper()
	path := d.rebuildGTPendingStore().Path()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "child"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

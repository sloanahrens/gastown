package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/installpending"
	"github.com/steveyegge/gastown/internal/version"
)

// pendingMarker returns the pending-install marker standing for the town, nil
// when none does.
func pendingMarker(t *testing.T, d *Daemon) *installpending.Marker {
	t.Helper()
	m, err := installpending.Pending(d.config.TownRoot, d.clk().Now())
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
	// A non-empty directory where the marker belongs: the write cannot rename
	// over it and the clear cannot remove it.
	if err := os.MkdirAll(installpending.Path(d.config.TownRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installpending.Path(d.config.TownRoot), "child"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("an install beside an unusable marker path reached its verdict")
	}
	if installs := installCalls(cli); len(installs) != 1 {
		t.Fatalf("installs = %v, want the one the marker path must not block", installs)
	}
}

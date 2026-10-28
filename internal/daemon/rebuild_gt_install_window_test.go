package daemon

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/version"
)

// rebuildGTWindowCli installs a fake CLI answering the install call with reply
// and everything else a clean checkout needs.
func rebuildGTWindowCli(t *testing.T, d *Daemon, reply func(c cliCall) cliReply) *fakeCLI {
	t.Helper()
	return withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case gitSub(c, "log"):
			return cliReply{stdout: "abc1234 the commit that was inert\n"}
		case c.name == "bash":
			return reply(c)
		}
		return cliReply{}
	})
}

// triggerInstallOnce arms a plain install request and runs it to completion,
// the way a landing pass and the next heartbeat do.
func triggerInstallOnce(t *testing.T, d *Daemon) {
	t.Helper()
	d.requestRebuildGTInstall()
	end := watchRebuildGTCycle(d)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, end)
}

// TestRebuildGTNoopDoesNotStartTheInstallWindow: the install window runs from
// an install that built and swapped a binary. A cycle that found the installed
// binary already contained the commit settled without installing anything, and
// anchoring the window on it held the next request over a build that never
// happened — the 2026-10-28 wait (gt-t135z).
func TestRebuildGTNoopDoesNotStartTheInstallWindow(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.clock = newFixedClock()
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	cli := rebuildGTWindowCli(t, d, func(cliCall) cliReply {
		return cliReply{stdout: "install-gt: RESULT noop abc1234567890 beefbeef already-installed\n"}
	})

	triggerInstallOnce(t, d)
	if got := len(installCalls(cli)); got != 1 {
		t.Fatalf("installs after the first request = %d, want 1", got)
	}
	if _, recorded := d.rebuildGTInstallAge(d.clk().Now()); recorded {
		t.Fatal("a noop install was recorded as an accomplished install")
	}

	// The next heartbeat, with no time passed: the noop must not hold it.
	triggerInstallOnce(t, d)
	if got := len(installCalls(cli)); got != 2 {
		t.Fatalf("installs after a noop = %d, want 2: the noop started the install window", got)
	}
}

// TestRebuildGTFailedInstallDoesNotStartTheInstallWindow: this is the
// 2026-10-28 wait. A failed install-gt is a settled verdict, so the patrol
// recorded a run; reading the window from that record anchored 30 minutes on a
// build that never swapped a binary, and the next landing's request waited it
// out (gt-t135z).
func TestRebuildGTFailedInstallDoesNotStartTheInstallWindow(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.clock = newFixedClock()
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	cli := rebuildGTWindowCli(t, d, func(cliCall) cliReply {
		// The image step failing under POLECAT_IMAGE_FAIL_CLOSED=1 is the shape
		// the 10-28 cycle reached (scripts/install-gt.sh).
		return cliReply{stdout: "install-gt: RESULT failed - - build-failed\n", code: 1}
	})

	triggerInstallOnce(t, d)
	if got := len(installCalls(cli)); got != 1 {
		t.Fatalf("installs after the failed install = %d, want 1", got)
	}
	if _, found, err := loadPatrolLastRun(d.config.TownRoot, "rebuild_gt"); err != nil || !found {
		t.Fatalf("the failed cycle left no patrol last-run record (found=%v err=%v), so this test cannot catch the window", found, err)
	}
	if _, recorded := d.rebuildGTInstallAge(d.clk().Now()); recorded {
		t.Fatal("a failed install was recorded as an accomplished install")
	}

	triggerInstallOnce(t, d)
	if got := len(installCalls(cli)); got != 2 {
		t.Fatalf("installs after a failed install = %d, want 2: the failed cycle started the install window", got)
	}
}

// TestRebuildGTDeferralDoesNotStartTheInstallWindow: install-gt exit 3
// accomplished nothing, so the request it kept goes straight back to work
// rather than waiting out a window no install started (gt-t135z).
func TestRebuildGTDeferralDoesNotStartTheInstallWindow(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.clock = newFixedClock()
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	// A settled cycle five minutes ago: the patrol is not due, so the install
	// window is the only thing that can hold the request below.
	if err := savePatrolLastRun(d.config.TownRoot, "rebuild_gt", d.clk().Now().Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var cli *fakeCLI
	cli = rebuildGTWindowCli(t, d, func(cliCall) cliReply {
		// The current call is already recorded, so one is this first install.
		if len(installCalls(cli)) == 1 {
			return cliReply{stdout: "install-gt: RESULT refused - - slot-busy\n", code: 3}
		}
		return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
	})

	triggerInstallOnce(t, d)
	if got := len(installCalls(cli)); got != 1 {
		t.Fatalf("installs during the deferral = %d, want 1", got)
	}
	if !d.rebuildGTRequested.Load() {
		t.Fatal("the deferred request was dropped")
	}
	if _, recorded := d.rebuildGTInstallAge(d.clk().Now()); recorded {
		t.Fatal("a deferral was recorded as an accomplished install")
	}

	// The retry the put-back request asks for, with no time passed.
	triggerInstallOnce(t, d)
	if got := len(installCalls(cli)); got != 2 {
		t.Fatalf("installs after the retry = %d, want 2: the deferral started the install window", got)
	}
}

// TestRebuildGTAccomplishedInstallStartsTheWindow: the one outcome that builds
// and swaps a binary is what the window measures from, so the request behind it
// waits (gt-t135z).
func TestRebuildGTAccomplishedInstallStartsTheWindow(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	clk := newFixedClock()
	d.clock = clk
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	cli := rebuildGTWindowCli(t, d, func(cliCall) cliReply {
		return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
	})

	triggerInstallOnce(t, d)
	if got := len(installCalls(cli)); got != 1 {
		t.Fatalf("installs after the first request = %d, want 1", got)
	}
	age, recorded := d.rebuildGTInstallAge(d.clk().Now())
	if !recorded || age != 0 {
		t.Fatalf("install age = %s recorded=%v after an accomplished install, want 0 and recorded", age, recorded)
	}

	triggerInstallOnce(t, d)
	if got := len(installCalls(cli)); got != 1 {
		t.Fatalf("installs inside the window = %d, want 1: an accomplished install must hold the next request", got)
	}
	if !d.rebuildGTRequested.Load() {
		t.Fatal("the held request was dropped")
	}

	// Past the window the held request installs.
	clk.Advance(defaultRebuildGTMinInterval)
	triggerInstallOnce(t, d)
	if got := len(installCalls(cli)); got != 2 {
		t.Fatalf("installs past the window = %d, want 2", got)
	}
}

package daemon

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/version"
)

// rebuildGTTown builds a town with a gt source checkout, a fake CLI answering
// every git and bash call, and a recorder for the escalations.
func rebuildGTTown(t *testing.T) (*Daemon, *notifyfake.Recorder) {
	t.Helper()
	d, rec := daemonWithRecorder(t)
	repoRoot := filepath.Join(d.config.TownRoot, "gastown")
	mkdirs(t, filepath.Join(repoRoot, "cmd", "gt"), filepath.Join(repoRoot, "scripts"))
	writeFile(t, filepath.Join(repoRoot, "cmd", "gt", "main.go"), "package main\n")
	// The fake CLI answers every bash call and rebuildGTInstall only stats the
	// path, so the installer is inert text: a shebang here would make the file
	// look like an executable, which the test policy forbids (no-exec-files).
	writeFile(t, filepath.Join(repoRoot, "scripts", "install-gt.sh"), "stub: the fake CLI answers this call\n")
	return d, rec
}

// withRebuildGTCli installs a fakeCLI answering the git and install calls a
// cycle makes. Everything unrecognized succeeds with no output, which is what
// a clean checkout and a silent install look like.
func withRebuildGTCli(t *testing.T, d *Daemon, answer func(c cliCall) cliReply) *fakeCLI {
	t.Helper()
	cli := newFakeCLIFor(answer)
	d.execCmd = cli.run
	return cli
}

// installCalls returns every install-gt.sh invocation the cycle made.
func installCalls(cli *fakeCLI) [][]string {
	var out [][]string
	for _, c := range cli.recorded() {
		if c.name == "bash" {
			out = append(out, c.args)
		}
	}
	return out
}

// gitSub reports whether the call is `git -C <dir> <sub> ...`.
func gitSub(c cliCall, sub string) bool {
	return c.name == "git" && len(c.args) >= 3 && c.args[0] == "-C" && c.args[2] == sub
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// staleInfo is one staleness reading: stale, forward-only, on a build branch,
// n commits behind.
func staleInfo(behind int) *version.StaleBinaryInfo {
	return &version.StaleBinaryInfo{IsStale: true, IsForward: true, OnMainBranch: true, CommitsBehind: behind, BinaryCommit: "beefbeef"}
}

// TestRebuildGTCycle_InstallsWhenDueAndQuiet is the case the job exists for: a
// merged commit that is not in force, a quiet town, and one install through
// the town's own script.
func TestRebuildGTCycle_InstallsWhenDueAndQuiet(t *testing.T) {
	t.Parallel()
	d, rec := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case gitSub(c, "log"):
			return cliReply{stdout: "abc1234 the commit that was inert\n"}
		}
		if c.name == "bash" {
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("a completed install must hold the interval, not retry the next heartbeat")
	}

	installs := installCalls(cli)
	if len(installs) != 1 {
		t.Fatalf("install calls = %v, want exactly one", installs)
	}
	got := strings.Join(installs[0], " ")
	for _, want := range []string{"--source rebuild-gt", "--sha abc1234567890"} {
		if !strings.Contains(got, want) {
			t.Errorf("install argv %q does not carry %q", got, want)
		}
	}
	if s := strings.Join(installs[0], " "); strings.Contains(s, "--slot-role") {
		t.Errorf("a quiet town must not reserve a gate slot: %q", s)
	}
	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("a clean install escalated: %+v", esc)
	}
}

// TestRebuildGTCycle_DirtyCheckoutDoesNotInstall pins the pre-flight: tracked
// changes outside .beads/ mean the tree is not what main says it is, so the
// build would produce a binary from someone's half-finished work.
func TestRebuildGTCycle_DirtyCheckoutDoesNotInstall(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "status") {
			return cliReply{stdout: " M internal/daemon/daemon.go\n"}
		}
		return cliReply{}
	})

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("a refusal a retry cannot fix holds the interval")
	}
	if installs := installCalls(cli); len(installs) != 0 {
		t.Errorf("a dirty checkout was built from: %v", installs)
	}
}

// TestRebuildGTCycle_DefersWhileAGateHoldsTheSlot pins the yield: `make build`
// competes for CPU with a load-sensitive suite, so a busy gate defers the
// build to the next heartbeat rather than racing it (gt-htx3).
func TestRebuildGTCycle_DefersWhileAGateHoldsTheSlot(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "a gate suite holds a slot (gastown/landing)", true }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "branch") {
			return cliReply{stdout: "main\n"}
		}
		return cliReply{}
	})

	if settled := d.runRebuildGT(); settled {
		t.Fatal("a deferral must leave the gate open so the next heartbeat retries")
	}
	if installs := installCalls(cli); len(installs) != 0 {
		t.Errorf("built while a gate held the slot: %v", installs)
	}
}

// TestRebuildGTCycle_WaitsOutAGatePastTheStarveCliff pins the reserve path: a
// due binary blocked past 30m on a slot it can queue for waits for that slot
// and builds inside it, rather than racing the suite it is yielding to
// (gt-kox0).
func TestRebuildGTCycle_WaitsOutAGatePastTheStarveCliff(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }

	// The gate is busy for the decision and free for the wait's first read,
	// so the reserve is granted on this cycle rather than after a sleep.
	gates := 0
	d.rebuildGTGateFn = func() (string, bool) {
		gates++
		if gates == 1 {
			return "a gate suite holds a slot (gastown/landing)", true
		}
		return "", false
	}
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case c.name == "bash":
			return cliReply{stdout: "install-gt: RESULT installed abc123 - -\n"}
		}
		return cliReply{}
	})
	// The block is already 31 minutes old: this is the starvation case.
	d.rebuildGTBlock.open(d.clk().Now().Add(-31 * time.Minute))

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("a completed install holds the interval")
	}
	installs := installCalls(cli)
	if len(installs) != 1 {
		t.Fatalf("install calls = %v, want one", installs)
	}
	got := strings.Join(installs[0], " ")
	if !strings.Contains(got, "--slot-role "+rebuildGTSlotRole) {
		t.Errorf("install argv %q does not reserve the gate slot", got)
	}
	idx := slices.Index(installs[0], "--slot-timeout")
	if idx < 0 || idx+1 >= len(installs[0]) {
		t.Fatalf("install argv %q has no --slot-timeout", got)
	}
	if secs, err := strconv.Atoi(installs[0][idx+1]); err != nil || secs <= 0 {
		t.Errorf("--slot-timeout = %q, want the seconds left of the reserve budget", installs[0][idx+1])
	}
}

// TestRebuildGTCycle_DeferredInstallRetriesNextHeartbeat pins install-gt's exit
// 3: a lock or slot it could not take accomplished nothing, so the cycle must
// not spend the hour.
func TestRebuildGTCycle_DeferredInstallRetriesNextHeartbeat(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case c.name == "bash":
			return cliReply{stdout: "install-gt: RESULT refused - - slot-busy\n", code: 3}
		}
		return cliReply{}
	})

	if settled := d.runRebuildGT(); settled {
		t.Fatal("install-gt exit 3 accomplished nothing and must retry next heartbeat")
	}
}

// TestRebuildGTCycle_FailedFetchDoesNotReadAsFresh pins gt-oyrav (D4): the
// sync used to discard a failed fetch, which leaves origin/main where it was,
// so the fast-forward is a no-op and the staleness read after it is taken
// against the checkout's own ref. Both verdicts the cycle can then reach are
// wrong: a stale binary installs against an unverified main ref, and one that
// only reads fresh because the checkout is stale closes the starvation and
// drift alarms over work the fetch never confirmed. Nothing was accomplished,
// so the cycle must defer and the next heartbeat must retry.
func TestRebuildGTCycle_FailedFetchDoesNotReadAsFresh(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stale *version.StaleBinaryInfo
	}{
		{"stale", staleInfo(3)},
		{"fresh against the checkout's own ref", &version.StaleBinaryInfo{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec := rebuildGTTown(t)
			d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return tc.stale }
			d.rebuildGTGateFn = func() (string, bool) { return "", false }
			cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
				switch {
				case gitSub(c, "branch"):
					return cliReply{stdout: "main\n"}
				case gitSub(c, "fetch"):
					return cliReply{stderr: "fatal: could not read from remote repository\n", code: 128}
				}
				return cliReply{}
			})

			if settled := d.runRebuildGT(); settled {
				t.Fatal("a failed fetch accomplished nothing and must retry next heartbeat")
			}
			if installs := installCalls(cli); len(installs) != 0 {
				t.Errorf("installed past a failed fetch, against an unverified main ref: %v", installs)
			}
			if clears := rec.Clears(); len(clears) != 0 {
				t.Errorf("a failed fetch closed the alarms over an unverified main ref: %+v", clears)
			}
			if esc := rec.Escalations(); len(esc) != 0 {
				t.Errorf("a deferral escalated: %+v", esc)
			}
		})
	}
}

// TestRebuildGTCycle_KilledInstallRetriesNextHeartbeat pins gt-oyrav (D5): an
// install the cycle's own budget killed (exit -1, no RESULT line) is not a
// verdict on the install, so taking it as one clears the sticky request and
// holds the hour over work that never happened. The request must survive.
func TestRebuildGTCycle_KilledInstallRetriesNextHeartbeat(t *testing.T) {
	t.Parallel()
	d, rec := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case c.name == "bash":
			return cliReply{stdout: "building gt...\n", code: -1}
		}
		return cliReply{}
	})
	// A cycle reached its verdict inside the patrol interval but past the
	// install window: only the drain request makes the job due again, so the
	// request is what the kill decides.
	if err := savePatrolLastRun(d.config.TownRoot, "rebuild_gt", d.clk().Now().Add(-45*time.Minute)); err != nil {
		t.Fatal(err)
	}
	d.requestRebuildGTInstall()
	cycleEnd := watchRebuildGTCycle(d)

	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, cycleEnd)

	if !d.rebuildGTRequested.Load() {
		t.Fatal("a killed install cleared the request; the next heartbeat will not retry")
	}
	if installs := installCalls(cli); len(installs) != 1 {
		t.Fatalf("installs = %v, want the one this cycle attempted", installs)
	}
	// A kill is the daemon's own clock, not an install-gt failure: escalating
	// here would page the operator for every slow build the budget cuts.
	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("a killed install escalated: %+v", esc)
	}
}

// TestRebuildGTCycle_FailedInstallEscalatesForInstallGt covers the failures
// install-gt does not escalate itself: its own fingerprints cover build,
// smoke, rollback and marker, and everything else reached nobody.
func TestRebuildGTCycle_FailedInstallEscalatesForInstallGt(t *testing.T) {
	t.Parallel()
	d, rec := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case c.name == "bash":
			return cliReply{stdout: "install-gt: RESULT failed - - unexpected\n", code: 1}
		}
		return cliReply{}
	})

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("a failed install has nothing to retry on the next heartbeat")
	}
	esc := rec.Escalations()
	if len(esc) != 1 || esc[0].Escalation.Fingerprint != alertKeyRebuildGTInstallFail {
		t.Fatalf("escalations = %+v, want one under %s", esc, alertKeyRebuildGTInstallFail)
	}
	if got := esc[0].Escalation.Severity; got != "MEDIUM" {
		t.Errorf("severity = %q, want MEDIUM (the build failure is install-gt's own HIGH)", got)
	}
}

// TestRebuildGTCycle_InstallGtsOwnFailureIsNotEscalatedTwice pins the split:
// install-gt escalates build/smoke/rollback/marker failures under install-gt:*,
// so escalating them again from here would mint a second bead for one failure.
func TestRebuildGTCycle_InstallGtsOwnFailureIsNotEscalatedTwice(t *testing.T) {
	t.Parallel()
	d, rec := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case c.name == "bash":
			return cliReply{stdout: "install-gt: RESULT failed - - smoke-failed\n", code: 1}
		}
		return cliReply{}
	})

	d.runRebuildGT()
	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("install-gt's own failure was escalated twice: %+v", esc)
	}
}

// TestRebuildGTCycle_FreshBinarySyncsBeforeBelievingItself pins gt-h8s8: the
// staleness check compares the binary against the checkout's own main ref, so
// a checkout that is itself behind origin makes a stale binary read as fresh.
// The sync has to run before the reading the cycle acts on.
func TestRebuildGTCycle_FreshBinarySyncsBeforeBelievingItself(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return &version.StaleBinaryInfo{} }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "branch") {
			return cliReply{stdout: "main\n"}
		}
		return cliReply{}
	})

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("a fresh binary reached a verdict")
	}
	merged := false
	for _, c := range cli.recorded() {
		if gitSub(c, "merge") {
			merged = true
		}
	}
	if !merged {
		t.Errorf("a fresh reading skipped the fast-forward: %v", cli.recorded())
	}
	if installs := installCalls(cli); len(installs) != 0 {
		t.Errorf("a fresh binary was installed over: %v", installs)
	}
}

// TestRebuildGTDrift_EscalatesTheOutcomeNotTheReason pins the drift alarm: it
// keys on the binary falling behind origin/main, so it fires on skip paths
// nobody has written yet (gt-bce).
func TestRebuildGTDrift_EscalatesTheOutcomeNotTheReason(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)

	d.rebuildGTDrift(d.config.TownRoot, staleInfo(rebuildGTMaxCommitsBehind+1))
	esc := d.notifier.(*notifyfake.Recorder).Escalations()
	if len(esc) != 1 || esc[0].Escalation.Fingerprint != alertKeyRebuildGTDrift {
		t.Fatalf("escalations = %+v, want one under %s", esc, alertKeyRebuildGTDrift)
	}
	if got := esc[0].Escalation.Severity; got != "MEDIUM" {
		t.Errorf("severity = %q, want MEDIUM", got)
	}

	// A stale binary whose count could not be read is not "0 behind": it could
	// be 1 commit or 1000 (gt-oqbw).
	d2, rec2 := daemonWithRecorder(t)
	d2.rebuildGTDrift(d2.config.TownRoot, staleInfo(0))
	esc = rec2.Escalations()
	if len(esc) != 1 || esc[0].Escalation.Fingerprint != alertKeyRebuildGTDriftUnknown {
		t.Fatalf("escalations = %+v, want one under %s", esc, alertKeyRebuildGTDriftUnknown)
	}

	// Under the ceiling and measurable: no alarm.
	if s := staleInfo(2); false {
		_ = s
	}
	d3, rec3 := daemonWithRecorder(t)
	d3.rebuildGTDrift(d3.config.TownRoot, staleInfo(2))
	if esc := rec3.Escalations(); len(esc) != 0 {
		t.Errorf("2 commits behind escalated: %+v", esc)
	}
}

// TestRebuildGTBlock_OneAlarmPerEpisode pins the starvation clock: the block
// ages across cycles, escalates once, and starts over when it closes.
func TestRebuildGTBlock_OneAlarmPerEpisode(t *testing.T) {
	t.Parallel()
	var b rebuildGTBlock
	start := time.Now()

	if age := b.open(start); age != 0 {
		t.Errorf("age at open = %s, want 0", age)
	}
	if age := b.open(start.Add(45 * time.Minute)); age != 45*time.Minute {
		t.Errorf("age on the next cycle = %s, want 45m", age)
	}
	if !b.alertOnce() {
		t.Error("the first alert past the cliff did not fire")
	}
	if b.alertOnce() {
		t.Error("a second alert fired for the same episode")
	}

	b.clear()
	if _, open := b.openFor(start.Add(time.Hour)); open {
		t.Error("a cleared block is still open")
	}
	if age := b.open(start.Add(2 * time.Hour)); age != 0 {
		t.Errorf("a new episode inherited the old start: age %s", age)
	}
	if !b.alertOnce() {
		t.Error("a new episode must be able to escalate")
	}
}

// TestRebuildGTInstallLog_ParsesTheResultLine pins the one machine-readable
// line the installer's contract is read from, including the "-" fields it
// writes for what it has nothing to say about.
func TestRebuildGTInstallLog_ParsesTheResultLine(t *testing.T) {
	t.Parallel()
	var w rebuildGTInstallLog
	_, _ = w.Write([]byte("building...\n"))
	_, _ = w.Write([]byte("# old line\n"))
	_, _ = w.Write([]byte("install-gt: RESULT installed abc123 beef456 -\n"))
	_, _ = w.Write([]byte("done\n"))

	got := w.result()
	if got.event != "installed" || got.commit != "abc123" || got.prev != "beef456" {
		t.Fatalf("result = %+v, want the last RESULT line", got)
	}
	if got.reason != "" {
		t.Errorf("reason = %q, want the - field read as empty", got.reason)
	}

	var empty rebuildGTInstallLog
	_, _ = empty.Write([]byte("install-gt: RESULT refused - - lock-busy\n"))
	if got := empty.result(); got.event != "refused" || got.commit != "" || got.prev != "" || got.reason != "lock-busy" {
		t.Errorf("result = %+v, want the - fields read as empty and the reason kept", got)
	}
}

// TestRebuildGTCycle_ReachedForceClosesTheAlarms pins the producer closing its
// own keys: every one of them asserts the binary is out of force somewhere,
// and an alarm with no closer outlives its cause (gt-vwry).
func TestRebuildGTCycle_ReachedForceClosesTheAlarms(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)
	cycle := d.startDogCycle("rebuild_gt")

	d.rebuildGTBlock.open(time.Now().Add(-time.Hour))
	d.rebuildGTReachedForce(cycle, "the binary is fresh")

	if _, open := d.rebuildGTBlock.openFor(time.Now()); open {
		t.Error("reaching force left the block open")
	}
	clears := rec.Clears()
	if len(clears) != 1 {
		t.Fatalf("clears = %+v, want one call", clears)
	}
	for _, key := range []string{alertKeyRebuildGTStarved, alertKeyRebuildGTDrift, alertKeyRebuildGTInstallFail} {
		if !slices.Contains(clears[0].Fingerprints, key) {
			t.Errorf("cleared keys %v do not carry %s", clears[0].Fingerprints, key)
		}
	}
}

// TestRebuildGTRequestDuringCycleSurvives pins the request's lifecycle across a
// cycle: the cycle consumes the request as it starts, so one a landing arms
// while the cycle runs is still pending when the cycle ends, rather than being
// erased with the one the cycle served (gt-j1mm4).
func TestRebuildGTRequestDuringCycleSurvives(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	inCycle := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
		once.Do(func() {
			close(inCycle)
			<-release
		})
		return &version.StaleBinaryInfo{}
	}
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "branch") {
			return cliReply{stdout: "main\n"}
		}
		return cliReply{}
	})
	d.requestRebuildGTInstall()
	cycleEnd := watchRebuildGTCycle(d)

	var releaseOnce sync.Once
	releaseCycle := func() { releaseOnce.Do(func() { close(release) }) }
	// A failed assertion below must not leave the cycle parked on release.
	t.Cleanup(func() {
		releaseCycle()
		awaitRebuildGTIdle(t, d, cycleEnd)
	})

	d.triggerRebuildGT()
	<-inCycle
	if d.rebuildGTRequested.Load() {
		t.Fatal("the cycle left the request armed while it ran, so a request made now is a no-op")
	}
	d.requestRebuildGTInstall()
	releaseCycle()
	awaitRebuildGTIdle(t, d, cycleEnd)

	if !d.rebuildGTRequested.Load() {
		t.Fatal("the cycle's end cleared a request armed while it ran")
	}
}

// TestRebuildGTRequestDuringDeferredCycleSurvives pins the deferral path with a
// request made mid-cycle: a cycle that defers (a gate holds the slot) hands the
// request it consumed back, and one a landing arms while it runs is still
// pending when the deferred cycle ends (gt-tgs81).
func TestRebuildGTRequestDuringDeferredCycleSurvives(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	inCycle := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
		once.Do(func() {
			close(inCycle)
			<-release
		})
		return staleInfo(3)
	}
	d.rebuildGTGateFn = func() (string, bool) { return "a gate suite holds a slot (gastown/landing)", true }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "branch") {
			return cliReply{stdout: "main\n"}
		}
		return cliReply{}
	})
	d.requestRebuildGTInstall()
	cycleEnd := watchRebuildGTCycle(d)

	var releaseOnce sync.Once
	releaseCycle := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseCycle()
		awaitRebuildGTIdle(t, d, cycleEnd)
	})

	d.triggerRebuildGT()
	<-inCycle
	d.requestRebuildGTInstall()
	releaseCycle()
	awaitRebuildGTIdle(t, d, cycleEnd)

	if installs := installCalls(cli); len(installs) != 0 {
		t.Errorf("built while a gate held the slot: %v", installs)
	}
	if !d.rebuildGTRequested.Load() {
		t.Fatal("a deferred cycle lost the request it consumed and the one armed while it ran")
	}
}

// TestRebuildGTCoalescesInstallsWithinMinInterval pins the install window: a
// landing's request installs once, and a second request inside the window
// leaves the request armed without a second build, while a gt:install-now
// landing bypasses the window and a request past it installs (gt-aiby1).
func TestRebuildGTCoalescesInstallsWithinMinInterval(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	clk := newFixedClock()
	d.clock = clk
	var logs lockedBuffer
	d.logger = log.New(&logs, "", 0)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case gitSub(c, "log"):
			return cliReply{stdout: "abc1234 the commit that was inert\n"}
		case c.name == "bash":
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})
	installs := func() int { return len(installCalls(cli)) }

	// The first landing's request installs.
	d.requestRebuildGTInstall()
	end := watchRebuildGTCycle(d)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, end)
	if got := installs(); got != 1 {
		t.Fatalf("installs after the first landing = %d, want 1", got)
	}

	// Ten minutes later a second landing is coalesced into the window: no
	// build, and the request stays armed for the window's end.
	clk.Advance(10 * time.Minute)
	d.requestRebuildGTInstall()
	d.triggerRebuildGT()
	if got := installs(); got != 1 {
		t.Fatalf("installs after 10 min = %d, want 1 (coalesced)", got)
	}
	if !d.rebuildGTRequested.Load() {
		t.Fatal("the held request was dropped")
	}
	// One line per wait: a second heartbeat inside the same window adds none.
	d.triggerRebuildGT()
	waitLine := "rebuild_gt: install requested 10m0s after the last install; waiting for the 30m0s window"
	if n := strings.Count(logs.String(), waitLine); n != 1 {
		t.Fatalf("window wait lines = %d, want 1:\n%s", n, logs.String())
	}

	// A gt:install-now landing bypasses the window.
	d.requestRebuildGTInstallNow()
	end = watchRebuildGTCycle(d)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, end)
	if got := installs(); got != 2 {
		t.Fatalf("installs after install-now = %d, want 2", got)
	}

	// Past the window the plain request installs without a bypass.
	clk.Advance(31 * time.Minute)
	d.requestRebuildGTInstall()
	end = watchRebuildGTCycle(d)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, end)
	if got := installs(); got != 3 {
		t.Fatalf("installs after 31 min = %d, want 3", got)
	}
}

// rebuildGTWindowTown is a rebuild_gt daemon on a fixed clock whose cycle
// installs on demand: a stale binary, a quiet gate, and a fake CLI answering
// the calls one install makes.
func rebuildGTWindowTown(t *testing.T) (*Daemon, *fakeCLI) {
	t.Helper()
	d, _ := rebuildGTTown(t)
	d.clock = newFixedClock()
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case gitSub(c, "log"):
			return cliReply{stdout: "abc1234 the commit that was inert\n"}
		case c.name == "bash":
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})
	return d, cli
}

// TestRebuildGTMinIntervalEdge pins the window's edge: a request one second
// inside the window waits, and one exactly at it installs — the window is a
// duration to be reached, not an exclusive bound on the last install.
func TestRebuildGTMinIntervalEdge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		age   time.Duration
		want  int
		armed bool
	}{
		{name: "one second inside the window", age: defaultRebuildGTMinInterval - time.Second, want: 0, armed: true},
		{name: "exactly at the window", age: defaultRebuildGTMinInterval, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, cli := rebuildGTWindowTown(t)
			if err := savePatrolLastRun(d.config.TownRoot, "rebuild_gt", d.clk().Now().Add(-tc.age)); err != nil {
				t.Fatal(err)
			}
			d.requestRebuildGTInstall()
			end := watchRebuildGTCycle(d)
			d.triggerRebuildGT()
			awaitRebuildGTIdle(t, d, end)

			if got := len(installCalls(cli)); got != tc.want {
				t.Errorf("installs = %d, want %d", got, tc.want)
			}
			if got := d.rebuildGTRequested.Load(); got != tc.armed {
				t.Errorf("request armed = %v, want %v", got, tc.armed)
			}
		})
	}
}

// TestRebuildGTUnreadableLastRunInstalls pins the window's failed-read
// branch: a last-run record that cannot be read makes the job due (the
// interval path's documented choice over silent skipping), so the requested
// install runs rather than being held on state nothing could compare. The
// unreadable state is reported, once.
func TestRebuildGTUnreadableLastRunInstalls(t *testing.T) {
	t.Parallel()
	d, cli := rebuildGTWindowTown(t)
	var logs lockedBuffer
	d.logger = log.New(&logs, "", 0)
	path := patrolLastRunPath(d.config.TownRoot)
	mkdirs(t, filepath.Dir(path))
	writeFile(t, path, "{not json")

	d.requestRebuildGTInstall()
	end := watchRebuildGTCycle(d)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, end)

	if got := len(installCalls(cli)); got != 1 {
		t.Fatalf("installs = %d, want 1: an unreadable window must not hold the request", got)
	}
	if d.rebuildGTRequested.Load() {
		t.Error("the request stayed armed after the install")
	}
	if !strings.Contains(logs.String(), "last-run state unreadable") {
		t.Errorf("the unreadable state was not reported:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "waiting for the") {
		t.Errorf("the window held a request on state it could not read:\n%s", logs.String())
	}
}

// TestRebuildGTInstallNowTravelsWithItsRequest pins the request pair: the
// gt:install-now marker is armed and consumed with the request it belongs to,
// so a plain request can never inherit a bypass, and a bypass can never
// outlive the request that carried it.
func TestRebuildGTInstallNowTravelsWithItsRequest(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)

	d.requestRebuildGTInstall()
	if armed, installNow := d.takeRebuildGTRequest(); !armed || installNow {
		t.Fatalf("plain request consumed as armed=%v install-now=%v, want true/false", armed, installNow)
	}
	if d.rebuildGTRequested.Load() || d.rebuildGTInstallNow.Load() {
		t.Fatal("consuming a request left a flag behind")
	}

	d.requestRebuildGTInstallNow()
	if armed, installNow := d.takeRebuildGTRequest(); !armed || !installNow {
		t.Fatalf("install-now request consumed as armed=%v install-now=%v, want true/true", armed, installNow)
	}

	// An install-now landing upgrades a plain request still pending, and the
	// next plain request after it is plain again.
	d.requestRebuildGTInstall()
	d.requestRebuildGTInstallNow()
	if armed, installNow := d.takeRebuildGTRequest(); !armed || !installNow {
		t.Fatalf("upgraded request consumed as armed=%v install-now=%v, want true/true", armed, installNow)
	}
	d.requestRebuildGTInstall()
	if armed, installNow := d.takeRebuildGTRequest(); !armed || installNow {
		t.Fatalf("plain request after install-now consumed as armed=%v install-now=%v, want true/false", armed, installNow)
	}
}

// TestRebuildGTDeferredInstallNowKeepsItsBypass pins the put-back: a
// gt:install-now request a gate deferred goes back as install-now, so the
// retry installs inside the window the label skips instead of waiting it out.
func TestRebuildGTDeferredInstallNowKeepsItsBypass(t *testing.T) {
	t.Parallel()
	d, cli := rebuildGTWindowTown(t)
	// An install five minutes ago: a plain request would be held.
	if err := savePatrolLastRun(d.config.TownRoot, "rebuild_gt", d.clk().Now().Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	d.rebuildGTGateFn = func() (string, bool) { return "a gate suite holds a slot (gastown/landing)", true }
	d.requestRebuildGTInstallNow()
	end := watchRebuildGTCycle(d)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, end)

	if got := len(installCalls(cli)); got != 0 {
		t.Fatalf("installs while the gate held the slot = %d, want 0", got)
	}
	if armed, installNow := d.rebuildGTPendingRequest(); !armed || !installNow {
		t.Fatalf("deferred request is armed=%v install-now=%v, want true/true", armed, installNow)
	}

	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	end = watchRebuildGTCycle(d)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, end)
	if got := len(installCalls(cli)); got != 1 {
		t.Fatalf("installs after the gate cleared = %d, want 1 (bypass kept)", got)
	}
}

// TestRebuildGTRequestSurvivesAnExitingCycle pins the request when the cycle
// exits without returning: runtime.Goexit runs the cycle's deferred calls the
// way a panic would, and the request the cycle consumed must still be pending
// for the next heartbeat rather than lost with the goroutine (gt-tgs81).
func TestRebuildGTRequestSurvivesAnExitingCycle(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
		runtime.Goexit()
		return nil
	}
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "branch") {
			return cliReply{stdout: "main\n"}
		}
		return cliReply{}
	})
	d.requestRebuildGTInstall()
	cycleEnd := watchRebuildGTCycle(d)

	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, cycleEnd)

	if !d.rebuildGTRequested.Load() {
		t.Fatal("a cycle that exited without deferring lost the request it consumed")
	}
}

// rebuildGTLogListing renders the `git log` listing rebuildGTBinaryAffectingCount
// reads: one record per commit, each carrying the paths that commit changed.
func rebuildGTLogListing(commits ...[]string) string {
	var b strings.Builder
	for i, paths := range commits {
		b.WriteString("\x1f" + strconv.Itoa(i+1) + "\n\n")
		for _, p := range paths {
			b.WriteString(p + "\n")
		}
	}
	return b.String()
}

// TestRebuildGTDrift_CountsEmbeddedMarkdownBacklog pins agreement with `gt
// stale`: markdown under an embed directory is compiled into the binary, so a
// backlog of commits that touch only that markdown is binary-affecting and
// escalates above the ceiling. The pathspec count read it as 0 (gt-9euk3).
func TestRebuildGTDrift_CountsEmbeddedMarkdownBacklog(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)
	commits := make([][]string, rebuildGTMaxCommitsBehind+1)
	for i := range commits {
		commits[i] = []string{"internal/templates/polecat-CLAUDE.md"}
	}
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "log"):
			return cliReply{stdout: rebuildGTLogListing(commits...)}
		case gitSub(c, "rev-list"):
			// What the old pathspec excludes counted for this range.
			return cliReply{stdout: "0\n"}
		}
		return cliReply{}
	})

	d.rebuildGTDrift(d.config.TownRoot, nonBinaryStale(len(commits), "cafecafe"))

	esc := rec.Escalations()
	if len(esc) != 1 || esc[0].Escalation.Fingerprint != alertKeyRebuildGTDrift {
		t.Fatalf("escalations = %+v, want one under %s", esc, alertKeyRebuildGTDrift)
	}
	if want := "binary is " + strconv.Itoa(len(commits)) + " commits behind"; !strings.Contains(esc[0].Escalation.Reason, want) {
		t.Errorf("reason = %q, want it to contain %q", esc[0].Escalation.Reason, want)
	}
}

// TestRebuildGTDrift_CountsNoCommitOutsideTheBuildPaths pins the other half: a
// backlog whose commits touch only documentation, markdown outside the embed
// directories, test sources and testdata counts 0, however long it is (gt-9euk3).
func TestRebuildGTDrift_CountsNoCommitOutsideTheBuildPaths(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)
	commits := make([][]string, rebuildGTMaxCommitsBehind+5)
	for i := range commits {
		switch i % 3 {
		case 0:
			commits[i] = []string{"docs/design/architecture.md"}
		case 1:
			commits[i] = []string{"README.md", "internal/daemon/rebuild_gt_test.go"}
		default:
			commits[i] = []string{"internal/daemon/testdata/fixture.txt"}
		}
	}
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "log") {
			return cliReply{stdout: rebuildGTLogListing(commits...)}
		}
		return cliReply{}
	})

	d.rebuildGTDrift(d.config.TownRoot, nonBinaryStale(len(commits), "cafecafe"))

	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("a backlog outside the build paths escalated: %+v", esc)
	}
}

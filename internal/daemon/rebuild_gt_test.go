package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

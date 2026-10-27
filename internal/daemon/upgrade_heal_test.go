package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/templates"
)

// fakeHealJob is the job the heal tests arm under. Its label is not the
// daemon's, so no test can reach com.gastown.daemon.
var fakeHealJob = upgradeHealJob{label: "com.example.fake", uid: 501, plist: "/fake/com.example.fake.plist"}

// recordHealRun returns a runner that records every argv and fails the ones
// failFor names (by argv[1]) with err.
func recordHealRun(failFor string, err error) (*[][]string, func([]string) error) {
	var calls [][]string
	return &calls, func(argv []string) error {
		calls = append(calls, append([]string(nil), argv...))
		if failFor != "" && len(argv) > 1 && argv[1] == failFor {
			return err
		}
		return nil
	}
}

func TestUpgradeRestartHealArmSubmitsHelperAsItsOwnJob(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	calls, run := recordHealRun("", nil)

	if err := submitUpgradeHeal(run, d.config.TownRoot, fakeHealJob); err != nil {
		t.Fatalf("submitUpgradeHeal: %v", err)
	}
	script := filepath.Join(d.config.TownRoot, "daemon", upgradeHealScriptName)
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("helper script not written: %v", err)
	}
	want := "launchctl submit -l com.example.fake-heal -- /bin/sh " + script
	if got := strings.Join((*calls)[len(*calls)-1], " "); got != want {
		t.Fatalf("last launchctl call = %q, want %q", got, want)
	}
	if got := strings.Join((*calls)[0], " "); got != "launchctl remove com.example.fake-heal" {
		t.Fatalf("first launchctl call = %q, want a remove of any stale helper", got)
	}
}

// TestUpgradeRestartHealArmClearsAnEarlierSignalStop checks a stop recorded
// before the arm does not excuse the helper: only a stop made while it waits
// may leave the daemon down (gt-r8ux8).
func TestUpgradeRestartHealArmClearsAnEarlierSignalStop(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	if err := recordSignalStop(d.config.TownRoot, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	_, run := recordHealRun("", nil)

	if err := submitUpgradeHeal(run, d.config.TownRoot, fakeHealJob); err != nil {
		t.Fatalf("submitUpgradeHeal: %v", err)
	}
	if _, err := os.Stat(signalStopPath(d.config.TownRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an earlier signal-stop record survived the arm (stat err %v)", err)
	}
}

func TestUpgradeRestartHealArmWriteFailureSubmitsNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir() // no daemon directory under it: the script cannot be written
	calls, run := recordHealRun("", nil)

	err := submitUpgradeHeal(run, town, fakeHealJob)
	if err == nil || !strings.Contains(err.Error(), "writing heal script") {
		t.Fatalf("err = %v, want a heal script write failure", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("launchctl ran although the script was not written: %v", *calls)
	}
}

func TestUpgradeRestartHealSubmitFailureNamesAFailedStaleRemove(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	run := func(argv []string) error { return errors.New(argv[1] + " refused") }

	err := submitUpgradeHeal(run, d.config.TownRoot, fakeHealJob)
	if err == nil || !strings.Contains(err.Error(), "submit refused") || !strings.Contains(err.Error(), "remove refused") {
		t.Fatalf("err = %v, want the submit failure and the stale remove failure", err)
	}
}

func TestUpgradeRestartHealExitArmsBeforeTheRestartError(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	var logAtArm string
	calls, run := recordHealRun("", nil)
	d.seams.upgradeHealJob = func(string) (upgradeHealJob, bool) { return fakeHealJob, true }
	d.seams.upgradeHealRun = func(argv []string) error {
		if logAtArm == "" {
			logAtArm = logs.String()
		}
		return run(argv)
	}
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	if !d.checkUpgradeRestart(now.Add(time.Minute)) {
		t.Fatal("must restart once the marker is idle")
	}
	if err := d.exitForUpgradeIfRequested(&State{Running: true}, now.Add(time.Minute)); !errors.Is(err, ErrRestartForUpgrade) {
		t.Fatalf("err = %v, want ErrRestartForUpgrade", err)
	}
	if len(*calls) == 0 {
		t.Fatal("the heal helper was not armed before the exit")
	}
	if !strings.Contains(logAtArm, "Daemon shutting down") {
		t.Fatalf("the helper was armed before the shutdown, so its wait starts too early; log at arm time:\n%s", logAtArm)
	}
}

func TestUpgradeRestartHealExitArmsNothingWithoutARequest(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	calls, run := recordHealRun("", nil)
	d.seams.upgradeHealJob = func(string) (upgradeHealJob, bool) { return fakeHealJob, true }
	d.seams.upgradeHealRun = run

	if err := d.exitForUpgradeIfRequested(&State{Running: true}, time.Now()); err != nil {
		t.Fatalf("err = %v, want nil with no upgrade requested", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("launchctl ran with no upgrade requested: %v", *calls)
	}
}

func TestUpgradeRestartHealFailedArmIsLoggedAndDoesNotBlockTheExit(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	_, run := recordHealRun("submit", errors.New("Bad request"))
	d.seams.upgradeHealJob = func(string) (upgradeHealJob, bool) { return fakeHealJob, true }
	d.seams.upgradeHealRun = run
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	if !d.checkUpgradeRestart(now.Add(time.Minute)) {
		t.Fatal("must restart once the marker is idle")
	}
	if err := d.exitForUpgradeIfRequested(&State{Running: true}, now.Add(time.Minute)); !errors.Is(err, ErrRestartForUpgrade) {
		t.Fatalf("err = %v, want ErrRestartForUpgrade despite the failed arm", err)
	}
	if !strings.Contains(logs.String(), "could not arm the heal helper, restarting anyway") {
		t.Fatalf("failed arm not logged:\n%s", logs.String())
	}
}

//testpolicy:allow parallel — HOME is process-wide, and pointing it at the throwaway home here would point every parallel test's home lookup there too
func TestUpgradeRestartHealJobComesFromTheSupervisorLookup(t *testing.T) {
	if runtime.GOOS != "darwin" {
		//testpolicy:allow no-skip — the launchd supervisor lookup is darwin-only, so elsewhere there is nothing to look up
		t.Skip("the launchd supervisor lookup is darwin-only")
	}
	home := t.TempDir()
	//testpolicy:allow no-env — HOME is the input under test: LaunchdPlistPath derives the plist path from it, and the real one must never be reached
	t.Setenv("HOME", home)
	town := t.TempDir()
	plist, err := templates.LaunchdPlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("a plist for this town names the supervisor job", func(t *testing.T) {
		body := "<key>WorkingDirectory</key>\n<string>" + town + "</string>\n"
		if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		job, ok := upgradeHealSupervisorJob(town)
		if !ok {
			t.Fatal("no supervisor job found for the town its plist serves")
		}
		if job.label != templates.LaunchdLabel || job.plist != plist || job.uid != os.Getuid() {
			t.Fatalf("job = %+v, want label %q, plist %q, uid %d", job, templates.LaunchdLabel, plist, os.Getuid())
		}
	})

	t.Run("a plist for another town arms nothing", func(t *testing.T) {
		body := "<key>WorkingDirectory</key>\n<string>" + filepath.Join(town, "elsewhere") + "</string>\n"
		if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := upgradeHealSupervisorJob(town); ok {
			t.Fatal("armed a helper for a plist that serves another town")
		}
	})
}

// TestUpgradeRestartHealScriptHealsOnlyAJobWithNoPid runs the rendered helper
// with fake launchctl, lsof and sleep first on PATH: the real launchd is never
// reached.
func TestUpgradeRestartHealScriptHealsOnlyAJobWithNoPid(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		//testpolicy:allow no-skip — the helper is a /bin/sh script, which Windows cannot run
		t.Skip("the helper is a /bin/sh script")
	}
	cases := []struct {
		name        string
		pid         string // launchctl print reports this pid on every sample; empty means not loaded
		pidOnce     string // launchctl print reports this pid on the first sample only
		lockHolder  string // lsof reports this process holds daemon.lock
		stopMarker  bool   // a daemon stop was requested while the helper waited
		signalStop  bool   // the daemon was stopped by a signal while the helper waited
		requested   bool   // that signal stop was requested through gt: StopDaemon left the stop marker
		gtDownStop  bool   // gt down stopped the daemon while the helper waited: the marker and the signal record a real stop leaves
		noPid       bool   // launchctl print reports the job loaded with no pid
		printBroken bool   // launchctl print cannot run at all (exit 127)
		estop       bool   // the town E-stop went up while the helper waited
		wantBootout bool
		wantBoots   int
		wantLogLine string
		stuckLoaded bool   // launchctl print keeps reporting the job after bootout
		bootstrapOK string // the bootstrap attempt that first succeeds
		wantGiveUp  bool   // the helper exits 1 and tells the system log
	}{
		{name: "healthy job is left alone", pid: "123", wantLogLine: "healthy:"},
		{name: "live lock holder is left alone", lockHolder: "4242", wantLogLine: "left alone:"},
		{name: "unloaded job is healed", wantBootout: true, wantBoots: 2, wantLogLine: "bootstrapped:"},
		{name: "a pid that is gone at the re-check is healed", pidOnce: "123", wantBootout: true, wantBoots: 1, wantLogLine: "bootstrapped:", bootstrapOK: "1"},
		{name: "a stop marker left while the helper waited is left alone", stopMarker: true, wantLogLine: "a daemon stop was requested"},
		{name: "a signal stop that unloaded the job is left alone", signalStop: true, wantLogLine: "is not loaded (a signal stopped the daemon"},
		{name: "a signal stop gt did not request, with the job still loaded, is healed", signalStop: true, noPid: true, wantBootout: true, wantBoots: 2, wantLogLine: "bootstrapped:"},
		{name: "a signal stop gt requested is left alone with the job still loaded", signalStop: true, requested: true, noPid: true, wantLogLine: "a daemon stop was requested through gt"},
		{name: "a signal stop gt requested is left alone when the job is unloaded", signalStop: true, requested: true, wantLogLine: "a daemon stop was requested through gt"},
		{name: "an E-stop raised while the helper waited is left alone", estop: true, wantLogLine: "E-stop active"},
		{name: "a job that stays loaded after bootout is given up on", stuckLoaded: true, wantBootout: true, wantLogLine: "gave up: gui/501/com.example.fake still loaded", wantGiveUp: true},
		{name: "a bootstrap that never succeeds is given up on", bootstrapOK: "never", wantBootout: true, wantBoots: 60, wantLogLine: "gave up: bootstrap of /fake/com.example.fake.plist failed 60 times", wantGiveUp: true},
		{name: "a launchctl print that cannot run leaves the job alone", printBroken: true, wantLogLine: "launchctl print could not run"},
		{name: "gt down stop with the job loaded and no pid", noPid: true, gtDownStop: true, wantLogLine: "a daemon stop was requested through gt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := upgradeTestDaemon(t)
			bin := t.TempDir()
			calls := filepath.Join(bin, "calls.txt")
			tries := filepath.Join(bin, "tries.txt")
			writeFakeTool(t, bin, "launchctl", `echo "$*" >> "$HEAL_CALLS"
case "$1" in
print)
	if [ -n "$HEAL_PRINT_BROKEN" ]; then exit 127; fi
	if [ -e "$HEAL_BOOTED" ] && [ -z "$HEAL_STUCK" ]; then echo 'Could not find service' >&2; exit 113; fi
	if [ -n "$HEAL_PID_ONCE" ]; then
		n=$(cat "$HEAL_PRINTS" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "$HEAL_PRINTS"
		if [ "$n" -eq 1 ]; then printf 'gui/501/x = {\n\tpid = %s\n}\n' "$HEAL_PID_ONCE"; exit 0; fi
	fi
	if [ -n "$HEAL_PID" ]; then printf 'gui/501/x = {\n\tpid = %s\n}\n' "$HEAL_PID"; exit 0; fi
	if [ -n "$HEAL_STUCK" ] || [ -n "$HEAL_NO_PID" ]; then printf 'gui/501/x = {\n\tstate = waiting\n}\n'; exit 0; fi
	echo 'Could not find service' >&2; exit 113 ;;
bootout)
	: > "$HEAL_BOOTED" ;;
bootstrap)
	n=$(cat "$HEAL_TRIES" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$HEAL_TRIES"
	if [ "$n" -ge "$HEAL_BOOT_OK" ]; then exit 0; fi
	echo 'Bootstrap failed: 5: Input/output error' >&2; exit 5 ;;
esac
exit 0
`)
			writeFakeTool(t, bin, "lsof", `[ -n "$HEAL_LOCK_HOLDER" ] && echo "$HEAL_LOCK_HOLDER"; exit 0`)
			writeFakeTool(t, bin, "sleep", `exit 0`)
			sysLog := filepath.Join(bin, "syslog.txt")
			writeFakeTool(t, bin, "logger", `echo "$*" >> "$HEAL_SYSLOG"`)
			bootOK := tc.bootstrapOK
			switch bootOK {
			case "":
				bootOK = "2"
			case "never":
				bootOK = "999"
			}
			stuck := ""
			if tc.stuckLoaded {
				stuck = "1"
			}
			noPid := ""
			if tc.noPid {
				noPid = "1"
			}
			broken := ""
			if tc.printBroken {
				broken = "1"
			}
			if tc.stopMarker {
				if err := writeStopRequested(d.config.TownRoot, 4242, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if tc.signalStop {
				if err := recordSignalStop(d.config.TownRoot, time.Now(), tc.requested); err != nil {
					t.Fatal(err)
				}
			}
			if tc.gtDownStop {
				// gt down: StopDaemon leaves the marker, then the daemon's real
				// signal branch records the stop and exits 0 on the marker.
				if err := writeStopRequested(d.config.TownRoot, os.Getpid(), time.Now()); err != nil {
					t.Fatal(err)
				}
				if err := d.shutdownForSignal(syscall.SIGTERM, &State{Running: true}); err != nil {
					t.Fatalf("a gt down stop must exit without ErrUnrequestedStop, got %v", err)
				}
			}
			if tc.estop {
				if err := estop.Activate(d.config.TownRoot, estop.TriggerManual, "operator stop"); err != nil {
					t.Fatal(err)
				}
			}

			script := filepath.Join(d.config.TownRoot, "daemon", upgradeHealScriptName)
			if err := os.WriteFile(script, []byte(upgradeHealScript(fakeHealJob, d.config.TownRoot)), 0o600); err != nil {
				t.Fatal(err)
			}
			//testpolicy:allow no-subprocess — the test is the process boundary: the rendered helper is a /bin/sh script, run against fake launchctl, lsof and sleep
			cmd := exec.Command("/bin/sh", script)
			cmd.Env = []string{
				"PATH=" + bin + ":/usr/bin:/bin",
				"HEAL_CALLS=" + calls,
				"HEAL_TRIES=" + tries,
				"HEAL_PID=" + tc.pid,
				"HEAL_PRINT_BROKEN=" + broken,
				"HEAL_PID_ONCE=" + tc.pidOnce,
				"HEAL_PRINTS=" + filepath.Join(bin, "prints.txt"),
				"HEAL_LOCK_HOLDER=" + tc.lockHolder,
				"HEAL_STUCK=" + stuck,
				"HEAL_NO_PID=" + noPid,
				"HEAL_BOOTED=" + filepath.Join(bin, "booted.txt"),
				"HEAL_BOOT_OK=" + bootOK,
				"HEAL_SYSLOG=" + sysLog,
			}
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			switch {
			case tc.wantGiveUp && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1):
				t.Fatalf("giving up must exit 1, got %v:\n%s", err, out)
			case !tc.wantGiveUp && err != nil:
				t.Fatalf("helper exited with %v:\n%s", err, out)
			}

			callLog, _ := os.ReadFile(calls)
			if !strings.Contains(string(callLog), "remove com.example.fake-heal") {
				t.Fatalf("the helper did not remove its own job on the way out; launchctl calls:\n%s", callLog)
			}
			sysText, _ := os.ReadFile(sysLog)
			if gotSys := strings.Contains(string(sysText), "gave up:"); gotSys != tc.wantGiveUp {
				t.Fatalf("system log carries a give-up = %v, want %v:\n%s", gotSys, tc.wantGiveUp, sysText)
			}
			healLog, err := os.ReadFile(filepath.Join(d.config.TownRoot, "daemon", upgradeHealLogName))
			if err != nil {
				t.Fatalf("heal decisions not written to %s: %v", upgradeHealLogName, err)
			}
			gotBootout := strings.Contains(string(callLog), "bootout")
			if gotBootout != tc.wantBootout {
				t.Fatalf("bootout ran = %v, want %v; launchctl calls:\n%s\nheal log:\n%s", gotBootout, tc.wantBootout, callLog, healLog)
			}
			if got := strings.Count(string(callLog), "bootstrap gui/501 /fake/com.example.fake.plist"); got != tc.wantBoots {
				t.Fatalf("bootstrap attempts = %d, want %d; launchctl calls:\n%s", got, tc.wantBoots, callLog)
			}
			if tc.pidOnce != "" {
				if got := strings.Count(string(callLog), "print gui/501/com.example.fake"); got < 2 {
					t.Fatalf("pid samples = %d, want the first sample and a re-check; launchctl calls:\n%s", got, callLog)
				}
			}
			if tc.pid != "" {
				// The re-check is one extra sample, taken because the first
				// carried a pid, not a count that has to reach a constant.
				if got := strings.Count(string(callLog), "print gui/501/com.example.fake"); got != 2 {
					t.Fatalf("pid samples = %d, want exactly one sample and one re-check; launchctl calls:\n%s", got, callLog)
				}
			}

			if !strings.Contains(string(healLog), tc.wantLogLine) {
				t.Fatalf("heal log missing %q:\n%s", tc.wantLogLine, healLog)
			}
			if _, err := os.Stat(filepath.Join(d.config.TownRoot, "daemon", "daemon.log")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the helper wrote daemon.log (stat err %v); its trail must not depend on it", err)
			}
		})
	}
}

// TestShutdownForSignalRecordsTheStopTheHealHelperReads drives the daemon's
// own signal-shutdown branch, so the helper's "requested through gt" answer
// fails the build if that branch stops recording what the marker says
// (gt-oagz9).
func TestShutdownForSignalRecordsTheStopTheHealHelperReads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		requested bool   // StopDaemon left a marker for this pid before the signal
		wantKind  string // the record's second line, which the helper matches
		wantErr   error
		wantLog   bool
	}{
		{name: "a stop gt requested exits 0 and is recorded as requested", requested: true, wantKind: signalStopRequested},
		{name: "an unrequested signal exits 75 and is recorded as unrequested", wantKind: signalStopUnrequested, wantErr: ErrUnrequestedStop, wantLog: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := upgradeTestDaemon(t)
			var logs bytes.Buffer
			d.logger = log.New(&logs, "", 0)
			if tc.requested {
				if err := writeStopRequested(d.config.TownRoot, os.Getpid(), time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.shutdownForSignal(syscall.SIGTERM, &State{Running: true}); !errors.Is(err, tc.wantErr) {
				t.Fatalf("signal shutdown = %v, want %v", err, tc.wantErr)
			}
			data, err := os.ReadFile(signalStopPath(d.config.TownRoot))
			if err != nil {
				t.Fatalf("the heal helper has no signal-stop record to read: %v", err)
			}
			if fields := strings.Fields(string(data)); len(fields) != 2 || fields[1] != tc.wantKind {
				t.Fatalf("signal-stop record = %q, want a second line %q", data, tc.wantKind)
			}
			if got := strings.Contains(logs.String(), unrequestedStopLogLine); got != tc.wantLog {
				t.Fatalf("unrequested-stop line logged = %v, want %v:\n%s", got, tc.wantLog, logs.String())
			}
			saved, err := LoadState(d.config.TownRoot)
			if err != nil {
				t.Fatalf("the shutdown did not save the state it was given: %v", err)
			}
			if saved.Running {
				t.Fatal("the shutdown saved a state that still says Running")
			}
		})
	}
}

// TestUpgradeRestartHealWedgedLaunchctlIsBounded checks one launchctl call
// cannot hold the exit that arms the helper: a call that never returns is
// killed at the bound and reported as a deadline, so the daemon still exits 75
// (gt-csq9g). The kill is read from the process table, not inferred from
// elapsed time: the child records its pid before it stops, so the test can
// find that pid gone afterwards, which a wall-clock margin cannot show
// (gt-8chz5).
func TestUpgradeRestartHealWedgedLaunchctlIsBounded(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		//testpolicy:allow no-skip — /bin/sh is the wedged child; Windows has none
		t.Skip("the wedged child is a /bin/sh process")
	}
	// kill -STOP stops the shell itself, so the child never returns and never
	// starts an external tool: the unit tier refuses a PATH lookup of sleep.
	// It writes its pid first so the test can look for the process afterwards.
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	const bound = 5 * time.Second
	done := make(chan error, 1)
	go func() {
		done <- runUpgradeHealCommand(bound, []string{"/bin/sh", "-c", "echo $$ > " + pidFile + "; kill -STOP $$"})
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(time.Minute): //testpolicy:allow no-sleep — a hang guard only: the bound the call must enforce is the subject of the test, this fails a helper that never returns at all
		t.Fatal("runUpgradeHealCommand never returned: the wedged call was not bounded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	pidText, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("the wedged child never recorded its pid: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if convErr != nil {
		t.Fatalf("the wedged child wrote pid %q: %v", pidText, convErr)
	}
	// A stopped process still answers signal 0; only a kill makes it answer
	// ESRCH, so this is the call having killed the child rather than returned
	// beside it.
	if sigErr := syscall.Kill(pid, 0); !errors.Is(sigErr, syscall.ESRCH) {
		t.Fatalf("the wedged child %d was not killed at the bound (signal 0 = %v): the call returned without it", pid, sigErr)
	}
}

// writeFakeTool writes an executable /bin/sh stand-in for name into dir.
func writeFakeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	// A fork that runs while this write descriptor is open hands the child a copy
	// of it, and the kernel then refuses to exec the fake as text file busy. The
	// fork lock is held across the write and the close (gt-e96f7, dv-bums).
	//testpolicy:allow no-exec-files — the test is the process boundary: the helper script finds these fakes first on PATH
	syscall.ForkLock.Lock()
	err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	syscall.ForkLock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

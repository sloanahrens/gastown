package daemon

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

func TestUpgradeRestartHealExitArmsBeforeTheRestartError(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	calls, run := recordHealRun("", nil)
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
		t.Fatalf("err = %v, want ErrRestartForUpgrade", err)
	}
	if len(*calls) == 0 {
		t.Fatal("the heal helper was not armed before the exit")
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
		pid         string // launchctl print reports this pid; empty means not loaded
		lockHolder  string // lsof reports this process holds daemon.lock
		wantBootout bool
		wantBoots   int
		wantLogLine string
	}{
		{name: "healthy job is left alone", pid: "123", wantLogLine: "healthy:"},
		{name: "live lock holder is left alone", lockHolder: "4242", wantLogLine: "left alone:"},
		{name: "unloaded job is healed", wantBootout: true, wantBoots: 2, wantLogLine: "bootstrapped:"},
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
	if [ -n "$HEAL_PID" ]; then printf 'gui/501/x = {\n\tpid = %s\n}\n' "$HEAL_PID"; exit 0; fi
	echo 'Could not find service' >&2; exit 113 ;;
bootstrap)
	n=$(cat "$HEAL_TRIES" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$HEAL_TRIES"
	if [ "$n" -ge 2 ]; then exit 0; fi
	echo 'Bootstrap failed: 5: Input/output error' >&2; exit 5 ;;
esac
exit 0
`)
			writeFakeTool(t, bin, "lsof", `[ -n "$HEAL_LOCK_HOLDER" ] && echo "$HEAL_LOCK_HOLDER"; exit 0`)
			writeFakeTool(t, bin, "sleep", `exit 0`)

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
				"HEAL_LOCK_HOLDER=" + tc.lockHolder,
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("helper exited with %v:\n%s", err, out)
			}

			callLog, _ := os.ReadFile(calls)
			gotBootout := strings.Contains(string(callLog), "bootout")
			if gotBootout != tc.wantBootout {
				t.Fatalf("bootout ran = %v, want %v; launchctl calls:\n%s", gotBootout, tc.wantBootout, callLog)
			}
			if got := strings.Count(string(callLog), "bootstrap gui/501 /fake/com.example.fake.plist"); got != tc.wantBoots {
				t.Fatalf("bootstrap attempts = %d, want %d; launchctl calls:\n%s", got, tc.wantBoots, callLog)
			}

			healLog, err := os.ReadFile(filepath.Join(d.config.TownRoot, "daemon", upgradeHealLogName))
			if err != nil {
				t.Fatalf("heal decisions not written to %s: %v", upgradeHealLogName, err)
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

// writeFakeTool writes an executable /bin/sh stand-in for name into dir.
func writeFakeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	//testpolicy:allow no-exec-files — the test is the process boundary: the helper script finds these fakes first on PATH
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

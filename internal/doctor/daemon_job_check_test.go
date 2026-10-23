package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// jobCheck is a darwin check whose lock and launchd job read as given, canned:
// the launchctl print parsing is exercised in internal/templates.
func jobCheck(lockHeld bool, lockPID int, st templates.SupervisorState, provisioned bool) *DaemonJobCheck {
	c := NewDaemonJobCheck()
	c.goos = "darwin"
	c.running = func(string) (bool, int, error) { return lockHeld, lockPID, nil }
	c.job = func(string) (templates.SupervisorState, bool) { return st, provisioned }
	return c
}

// jobSpawnScheduled is the 2026-10-22 reading: launchd knew the job, had no
// process for it (state spawn scheduled, no pid line) and its last exit code
// was 78: EX_CONFIG.
var jobSpawnScheduled = templates.SupervisorState{
	Kind: "launchd", Loaded: true, PID: 0, Runs: 3, LastExit: templates.LaunchdExitConfig,
}

func TestDaemonJobCheck_DaemonRunning_OK(t *testing.T) {
	t.Parallel()
	st := templates.SupervisorState{Kind: "launchd", Loaded: true, PID: 27426, Runs: 1, LastExit: -1}

	result := jobCheck(true, 27426, st, true).Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "27426") {
		t.Errorf("message %q does not name the pid", result.Message)
	}
}

func TestDaemonJobCheck_NotProvisioned_OK(t *testing.T) {
	t.Parallel()
	for name, st := range map[string]templates.SupervisorState{
		"no supervisor": {Kind: "", LastExit: -1},
		"other town":    {Kind: "launchd", LastExit: -1},
		"systemd":       {Kind: "systemd", Loaded: true, LastExit: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result := jobCheck(false, 0, st, false).Run(&CheckContext{TownRoot: t.TempDir()})
			if result.Status != StatusOK {
				t.Errorf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
			}
		})
	}
}

// A plist that is installed but not bootstrapped is DaemonCheck's finding, not
// this check's.
func TestDaemonJobCheck_NotLoaded_OK(t *testing.T) {
	t.Parallel()
	st := templates.SupervisorState{Kind: "launchd", Loaded: false, LastExit: -1}

	result := jobCheck(false, 0, st, true).Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
	}
}

func TestDaemonJobCheck_SpawnScheduledExit78_Error(t *testing.T) {
	t.Parallel()

	result := jobCheck(false, 0, jobSpawnScheduled, true).Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError; message: %s", result.Status, result.Message)
	}
	for _, want := range []string{"not running", "last exit code 78", "EX_CONFIG"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q does not name %q", result.Message, want)
		}
	}
	for _, want := range []string{"launchctl bootout gui/", "/com.gastown.daemon && launchctl bootstrap gui/", "com.gastown.daemon.plist"} {
		if !strings.Contains(result.FixHint, want) {
			t.Errorf("FixHint %q does not carry %q", result.FixHint, want)
		}
	}
}

// A loaded job with no process and no exit status reported is still not
// running the daemon.
func TestDaemonJobCheck_NoProcessNoExitStatus_Error(t *testing.T) {
	t.Parallel()
	st := templates.SupervisorState{Kind: "launchd", Loaded: true, LastExit: -1}

	result := jobCheck(false, 0, st, true).Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError; message: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "unreported") {
		t.Errorf("message %q does not say the exit status is unreported", result.Message)
	}
}

// A job with a process that has not taken the lock yet is starting, not
// failing, whatever the run before it exited with.
func TestDaemonJobCheck_ProcessWithoutLock_Warning(t *testing.T) {
	t.Parallel()
	for name, lastExit := range map[string]int{
		"no exit reported": -1,
		"clean exit":       0,
		"EX_CONFIG":        templates.LaunchdExitConfig,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := templates.SupervisorState{Kind: "launchd", Loaded: true, PID: 501, Runs: 2, LastExit: lastExit}

			result := jobCheck(false, 0, st, true).Run(&CheckContext{TownRoot: t.TempDir()})

			if result.Status != StatusWarning {
				t.Errorf("Status = %v, want StatusWarning; message: %s", result.Status, result.Message)
			}
			if !strings.Contains(result.Message, "501") {
				t.Errorf("message %q does not name the pid", result.Message)
			}
		})
	}
}

// With no job reader injected, the check reads the host's launchd plist under
// the home directory. HOME points at a temp dir so the host's own plist and
// launchctl are never touched; the other-town plist is the one that must read
// as no job for this town.
func TestDaemonJobCheck_HostJobReader_OtherTownPlist_OK(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeLaunchdPlist(t, home, t.TempDir())

	townRoot := t.TempDir()
	if _, provisioned := hostDaemonJob(townRoot); provisioned {
		t.Fatalf("hostDaemonJob reports a job for another town as provisioned here")
	}

	c := NewDaemonJobCheck()
	c.goos = "darwin"
	result := c.Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
	}
}

// With no plist installed at all, the nil job reader is the same answer as no
// job for this town: the check passes.
func TestDaemonJobCheck_HostJobReader_NoPlist_OK(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	c := NewDaemonJobCheck()
	c.goos = "darwin"
	result := c.Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
	}
}

// writeLaunchdPlist installs a plist under home whose WorkingDirectory is
// workingDir, in the shape the launchd template renders.
func writeLaunchdPlist(t *testing.T, home, workingDir string) {
	t.Helper()
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := "<key>WorkingDirectory</key>\n<string>" + workingDir + "</string>\n"
	if err := os.WriteFile(filepath.Join(dir, "com.gastown.daemon.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonJobCheck_UnreadableState_Skipped(t *testing.T) {
	t.Parallel()
	st := templates.SupervisorState{Kind: "launchd", LastExit: -1, Err: errors.New("launchctl print: exec: not found")}

	result := jobCheck(false, 0, st, true).Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped; message: %s", result.Status, result.Message)
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "launchctl print") {
		t.Errorf("details %q do not carry the probe error", result.Details)
	}
}

func TestDaemonJobCheck_UnreadableLock_Skipped(t *testing.T) {
	t.Parallel()
	c := jobCheck(false, 0, jobSpawnScheduled, true)
	c.running = func(string) (bool, int, error) { return false, 0, errors.New("lock: permission denied") }

	result := c.Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusSkipped {
		t.Errorf("Status = %v, want StatusSkipped; message: %s", result.Status, result.Message)
	}
}

func TestDaemonJobCheck_NotDarwin_Skipped(t *testing.T) {
	t.Parallel()
	c := jobCheck(false, 0, jobSpawnScheduled, true)
	c.goos = "linux"
	c.job = func(string) (templates.SupervisorState, bool) {
		t.Error("the job was read on a host with no launchd")
		return templates.SupervisorState{}, false
	}

	result := c.Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusSkipped {
		t.Errorf("Status = %v, want StatusSkipped; message: %s", result.Status, result.Message)
	}
}

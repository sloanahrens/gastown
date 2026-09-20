package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubDaemonStart replaces the seams startDaemon uses — supervisor detection
// (GOOS + file paths), running the supervisor command, the direct spawn and
// the running check — and returns recorders for what was called.
func stubDaemonStart(t *testing.T, goos, file string, runErr error) (ran *[]string, spawned *bool) {
	t.Helper()
	prevPlist, prevUnit, prevRun, prevSpawn, prevIsRunning, prevGOOS := supervisorPlistPath, supervisorUnitPath, supervisorRun, spawnDaemonDirect, daemonIsRunning, supervisorGOOS
	t.Cleanup(func() {
		supervisorPlistPath, supervisorUnitPath, supervisorRun, spawnDaemonDirect, daemonIsRunning, supervisorGOOS = prevPlist, prevUnit, prevRun, prevSpawn, prevIsRunning, prevGOOS
	})
	supervisorGOOS = goos
	supervisorPlistPath = func() (string, error) { return file, nil }
	supervisorUnitPath = func() (string, error) { return file, nil }
	var argv []string
	ran = &argv
	spawnedFlag := false
	spawned = &spawnedFlag
	supervisorRun = func(a []string) error { argv = append([]string{}, a...); return runErr }
	spawnDaemonDirect = func(townRoot string) (int, error) { spawnedFlag = true; return 1234, nil }
	// Not running before the start, running after it.
	calls := 0
	daemonIsRunning = func(townRoot string) (bool, int, error) {
		calls++
		if calls == 1 {
			return false, 0, nil
		}
		return true, 4242, nil
	}
	return ran, spawned
}

// writeSupervisorFile writes a plist/unit naming town as WorkingDirectory,
// the way the two templates render it.
func writeSupervisorFile(t *testing.T, name, town string) string {
	t.Helper()
	var body string
	if strings.HasSuffix(name, ".plist") {
		body = "<plist><dict><key>WorkingDirectory</key>\n<string>" + town + "</string></dict></plist>"
	} else {
		body = "[Service]\nWorkingDirectory=" + town + "\nExecStart=/usr/local/bin/gt daemon run\n"
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// With a launchd plist provisioned for this town, gt daemon start must start
// the daemon THROUGH launchd (kickstart) and never spawn its own child: a
// manual daemon holds daemon.lock, and the KeepAlive job then respawns every
// ~10 s, loses the lock and exits 1 for as long as the manual one lives
// (gt-3jrm).
func TestStartDaemon_UsesLaunchdWhenProvisioned(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	ran, spawned := stubDaemonStart(t, "darwin", plist, nil)

	via, pid, err := startDaemon(town)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if *spawned {
		t.Fatal("spawned a direct daemon child although launchd is provisioned")
	}
	if via != "launchd" || pid != 4242 {
		t.Fatalf("startDaemon = (%q, %d), want (launchd, 4242)", via, pid)
	}
	got := strings.Join(*ran, " ")
	if !strings.HasPrefix(got, "launchctl kickstart -k gui/") || !strings.HasSuffix(got, "/com.gastown.daemon") {
		t.Fatalf("supervisor command = %q, want launchctl kickstart -k gui/<uid>/com.gastown.daemon", got)
	}
}

// No plist → the direct spawn, exactly as before.
func TestStartDaemon_SpawnsDirectlyWithoutSupervisor(t *testing.T) {
	ran, spawned := stubDaemonStart(t, "darwin", filepath.Join(t.TempDir(), "absent.plist"), nil)
	via, _, err := startDaemon(t.TempDir())
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if len(*ran) != 0 || via != "" {
		t.Fatalf("ran=%v via=%q with no supervisor provisioned", *ran, via)
	}
	if !*spawned {
		t.Fatal("did not spawn the daemon directly")
	}
}

// The plist is per user and names the town it was provisioned for; from a
// different workspace it must not be kickstarted (that would restart the
// other town's daemon and then wait for this one's lock in vain).
func TestStartDaemon_IgnoresSupervisorOfAnotherTown(t *testing.T) {
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", filepath.Join(t.TempDir(), "other-town"))
	ran, spawned := stubDaemonStart(t, "darwin", plist, nil)
	via, _, err := startDaemon(t.TempDir())
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if len(*ran) != 0 || via != "" || !*spawned {
		t.Fatalf("ran=%v via=%q spawned=%v; want the direct spawn, no supervisor", *ran, via, *spawned)
	}
}

// A plist that exists but whose job is not loaded (booted out) makes
// kickstart fail. The daemon must NOT be spawned by hand — a job that is in
// fact still loaded would respawn against it forever — and the error must
// say how to load the job.
func TestStartDaemon_SupervisorFailureIsAnErrorNotAManualSpawn(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	ran, spawned := stubDaemonStart(t, "darwin", plist, errors.New("Could not find service"))
	// The daemon never comes up: the stub's second answer would say running,
	// so make every answer "not running" for this case.
	daemonIsRunning = func(string) (bool, int, error) { return false, 0, nil }
	_, _, err := startDaemon(town)
	if err == nil {
		t.Fatal("startDaemon succeeded although launchd could not start the daemon")
	}
	if len(*ran) == 0 {
		t.Fatal("did not try the supervisor first")
	}
	if *spawned {
		t.Fatal("spawned a manual daemon beside a provisioned supervisor")
	}
	if !strings.Contains(err.Error(), "launchctl bootstrap gui/") || !strings.Contains(err.Error(), plist) {
		t.Fatalf("error does not say how to load the job: %v", err)
	}
}

// A supervisor command that reports failure may still have started the
// daemon; that is success, not an error.
func TestStartDaemon_SupervisorErrorButDaemonCameUp(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	_, spawned := stubDaemonStart(t, "darwin", plist, errors.New("kickstart: noise on stderr"))
	via, pid, err := startDaemon(town)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if via != "launchd" || pid != 4242 || *spawned {
		t.Fatalf("got (%q, %d, spawned=%v), want (launchd, 4242, false)", via, pid, *spawned)
	}
}

// A supervisor file that exists but cannot be read must not be mistaken
// for "no supervisor" — that is the one answer that recreates the loop.
func TestStartDaemon_UnreadableSupervisorFileIsAnError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read anything")
	}
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	if err := os.Chmod(plist, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(plist, 0o644) })
	ran, spawned := stubDaemonStart(t, "darwin", plist, nil)
	if _, _, err := startDaemon(town); err == nil {
		t.Fatal("startDaemon succeeded with an unreadable supervisor file")
	}
	if len(*ran) != 0 || *spawned {
		t.Fatalf("ran=%v spawned=%v; want neither with an unreadable supervisor file", *ran, *spawned)
	}
}

// On Linux the provisioned supervisor is a systemd user unit; the same
// decision routes through systemctl.
func TestStartDaemon_UsesSystemdWhenProvisioned(t *testing.T) {
	town := t.TempDir()
	unit := writeSupervisorFile(t, "gastown-daemon.service", town)
	ran, spawned := stubDaemonStart(t, "linux", unit, nil)
	via, _, err := startDaemon(town)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if *spawned || via != "systemd" {
		t.Fatalf("via=%q spawned=%v, want systemd and no direct spawn", via, *spawned)
	}
	if got := strings.Join(*ran, " "); got != "systemctl --user restart gastown-daemon.service" {
		t.Fatalf("supervisor command = %q", got)
	}
}

// An OS with no supervisor support spawns directly.
func TestStartDaemon_UnsupportedOSSpawnsDirectly(t *testing.T) {
	ran, spawned := stubDaemonStart(t, "windows", filepath.Join(t.TempDir(), "whatever"), nil)
	if _, _, err := startDaemon(t.TempDir()); err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if len(*ran) != 0 || !*spawned {
		t.Fatalf("ran=%v spawned=%v, want no supervisor command and a direct spawn", *ran, *spawned)
	}
}

// The town may be spelled differently from the provisioned file (a symlink
// to it, /tmp vs /private/tmp on macOS); the guard must still see the
// supervisor, or it hand-spawns beside a loaded job.
func TestStartDaemon_MatchesSupervisorThroughSymlink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "town-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", real)
	ran, spawned := stubDaemonStart(t, "darwin", plist, nil)
	via, _, err := startDaemon(link)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if via != "launchd" || *spawned || len(*ran) == 0 {
		t.Fatalf("via=%q spawned=%v ran=%v; want launchd through the symlinked town", via, *spawned, *ran)
	}
}

// A supervisor file with no WorkingDirectory (hand-edited, older format)
// is provisioned as far as launchd is concerned; it must be an error, not
// "no supervisor".
func TestStartDaemon_SupervisorFileWithoutWorkingDirectoryIsAnError(t *testing.T) {
	plist := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.WriteFile(plist, []byte("<plist><dict><key>Label</key><string>com.gastown.daemon</string></dict></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}
	ran, spawned := stubDaemonStart(t, "darwin", plist, nil)
	_, _, err := startDaemon(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "WorkingDirectory") {
		t.Fatalf("err = %v, want an error naming the missing WorkingDirectory", err)
	}
	if len(*ran) != 0 || *spawned {
		t.Fatalf("ran=%v spawned=%v; want neither", *ran, *spawned)
	}
}

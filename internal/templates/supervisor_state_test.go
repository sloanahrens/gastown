package templates

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// launchdPrintRunning is a trimmed capture of 'launchctl print
// gui/501/com.gastown.daemon' on a host whose daemon is the job's own child.
// The block form ("arguments = {") and the "key => value" lines are part of
// the real output and must not confuse the parser.
const launchdPrintRunning = "gui/501/com.gastown.daemon = {\n" +
	"\tactive count = 1\n" +
	"\tpath = /Users/sloan/Library/LaunchAgents/com.gastown.daemon.plist\n" +
	"\ttype = LaunchAgent\n" +
	"\tstate = running\n" +
	"\n" +
	"\tprogram = /Users/sloan/.local/bin/gt\n" +
	"\targuments = {\n" +
	"\t\t/Users/sloan/.local/bin/gt\n" +
	"\t\tdaemon\n" +
	"\t\trun\n" +
	"\t}\n" +
	"\n" +
	"\tworking directory = /Users/sloan/gt\n" +
	"\tminimum runtime = 10\n" +
	"\truns = 53\n" +
	"\tpid = 64604\n" +
	"\tstarted suspended = 0\n" +
	"\tlast exit code = 0\n" +
	"\n" +
	"\tsemaphores = {\n" +
	"\t\tsuccessful exit => 0\n" +
	"\t\tafter crash => 1\n" +
	"\t}\n"

// launchdPrintCrashLooping is the shape a job has between crashes: loaded and
// counting runs, with no pid line and a non-zero last exit code (gt-sq9e).
const launchdPrintCrashLooping = "gui/501/com.gastown.daemon = {\n" +
	"\tactive count = 0\n" +
	"\tstate = not running\n" +
	"\truns = 58\n" +
	"\tlast exit code = 1\n"

func TestParseLaunchdPrint(t *testing.T) {
	t.Parallel()
	loaded, pid, runs, lastExit := parseLaunchdPrint(launchdPrintRunning)
	if !loaded || pid != 64604 || runs != 53 || lastExit != 0 {
		t.Errorf("parseLaunchdPrint(running) = (%v, %d, %d, %d), want (true, 64604, 53, 0)", loaded, pid, runs, lastExit)
	}
}

// A job between crashes prints no pid: pid 0 with Loaded is "the supervisor
// has no process", which StatusLine reports as FAILING or DETACHED.
func TestParseLaunchdPrint_NoPIDIsZeroNotAnError(t *testing.T) {
	t.Parallel()
	loaded, pid, runs, lastExit := parseLaunchdPrint(launchdPrintCrashLooping)
	if !loaded || pid != 0 || runs != 58 || lastExit != 1 {
		t.Errorf("parseLaunchdPrint(crash-looping) = (%v, %d, %d, %d), want (true, 0, 58, 1)", loaded, pid, runs, lastExit)
	}
}

func TestParseSystemctlShow(t *testing.T) {
	t.Parallel()
	out := "LoadState=loaded\nMainPID=1234\nNRestarts=3\nExecMainStatus=0\n"
	loaded, pid, runs, lastExit := parseSystemctlShow(out)
	if !loaded || pid != 1234 || runs != 3 || lastExit != 0 {
		t.Errorf("parseSystemctlShow(loaded) = (%v, %d, %d, %d), want (true, 1234, 3, 0)", loaded, pid, runs, lastExit)
	}
}

func TestParseSystemctlShow_NotLoaded(t *testing.T) {
	t.Parallel()
	loaded, pid, _, lastExit := parseSystemctlShow("LoadState=not-found\nMainPID=0\nNRestarts=0\nExecMainStatus=0\n")
	if loaded || pid != 0 || lastExit != 0 {
		t.Errorf("parseSystemctlShow(not-found) = (%v, %d, %d), want (false, 0, 0)", loaded, pid, lastExit)
	}
}

// A probe that finds no such job is the ordinary "not loaded" reading, not a
// failure: launchctl exits 113 with this message when the label is not in the
// user's gui domain, which is where a booted-out job stays.
func TestSupervisorJobState_UnknownLaunchdJobIsNotAnError(t *testing.T) {
	stubSupervisorProbe(t, "Bad request.\nCould not find service \"com.gastown.daemon\" in domain for user gui: 501\n", errors.New("exit status 113"))

	st := SupervisorJobState("launchd")
	if st.Loaded || st.Err != nil {
		t.Errorf("SupervisorJobState(launchd) = %+v, want Loaded=false and no error", st)
	}
	if st.LastExit != -1 {
		t.Errorf("LastExit = %d, want -1 (unreported is not a clean exit)", st.LastExit)
	}
}

// A probe that fails for any other reason must not read as "no supervisor
// job": that would be the same lie in the other direction.
func TestSupervisorJobState_ProbeFailureIsAnError(t *testing.T) {
	stubSupervisorProbe(t, "", errors.New("exec: \"launchctl\": executable file not found in $PATH"))

	st := SupervisorJobState("launchd")
	if st.Err == nil || st.Loaded {
		t.Errorf("SupervisorJobState(launchd) = %+v, want an error and Loaded=false", st)
	}
}

func TestStatusLine(t *testing.T) {
	t.Parallel()
	failing := SupervisorState{Kind: "launchd", Loaded: true, Runs: 58, LastExit: 1}
	tests := []struct {
		name    string
		state   SupervisorState
		lockPID int
		want    string
	}{
		{"no supervisor", SupervisorState{Kind: "none"}, 4242, "none"},
		{"unreadable job", SupervisorState{Kind: "launchd", Err: errors.New("boom")}, 4242,
			"launchd (unverified - boom)"},
		{"job not loaded, no daemon", SupervisorState{Kind: "launchd"}, 0, "launchd (the job is not loaded)"},
		{"job not loaded, daemon by hand", SupervisorState{Kind: "launchd"}, 76426,
			"launchd (DETACHED - the job is not loaded; PID 76426 was started by hand)"},
		{"supervised and alive", SupervisorState{Kind: "launchd", Loaded: true, PID: 64604, Runs: 53, LastExit: 0}, 64604,
			"launchd"},
		{"crash-looping behind a hand-started daemon", failing, 76426,
			"launchd (DETACHED - launchd is running no daemon while PID 76426 holds the lock, runs=58, last exit code=1)"},
		{"another process under the lock", SupervisorState{Kind: "launchd", Loaded: true, PID: 21307, Runs: 58, LastExit: 1}, 76426,
			"launchd (DETACHED - launchd runs PID 21307, not the daemon holding the lock (PID 76426), runs=58, last exit code=1)"},
		{"job up, lock not taken", SupervisorState{Kind: "launchd", Loaded: true, PID: 21307, Runs: 53, LastExit: 0}, 0,
			"launchd (pid 21307, runs=53, last exit code=0)"},
		{"loaded, no daemon at all", failing, 0,
			"launchd (FAILING - loaded, running no daemon, runs=58, last exit code=1)"},
		{"loaded, never run", SupervisorState{Kind: "launchd", Loaded: true, LastExit: -1}, 0,
			"launchd (loaded, not started yet)"},
		{"systemd names itself", SupervisorState{Kind: "systemd", Loaded: true, Runs: 2, LastExit: 1}, 4242,
			"systemd (DETACHED - systemd is running no daemon while PID 4242 holds the lock, runs=2, last exit code=1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.state.StatusLine(tt.lockPID); got != tt.want {
				t.Errorf("StatusLine(%d) = %q, want %q", tt.lockPID, got, tt.want)
			}
		})
	}
}

// SupervisorStatusLine composes the file reading with the live one: a file
// installed for another town leaves this town unsupervised, whatever the job
// named in it is doing.
func TestSupervisorStatusLine(t *testing.T) {
	town := t.TempDir()
	other := t.TempDir()
	alive := SupervisorState{Kind: "", Loaded: true, PID: 64604, Runs: 53, LastExit: 0}
	read := func(kind string) SupervisorState {
		alive.Kind = kind
		return alive
	}

	t.Run("served by this town's supervisor", func(t *testing.T) {
		writeHostSupervisorFile(t, town)
		if got := SupervisorStatusLine(town, 64604, read); got == "none" || strings.Contains(got, "DETACHED") {
			t.Errorf("SupervisorStatusLine = %q, want the bare kind for the daemon the job runs", got)
		}
	})

	t.Run("file of another town", func(t *testing.T) {
		writeHostSupervisorFile(t, other)
		if got := SupervisorStatusLine(town, 64604, read); got != "none" {
			t.Errorf("SupervisorStatusLine = %q, want %q for a file naming another town", got, "none")
		}
	})

	t.Run("no file", func(t *testing.T) {
		isolateHome(t)
		if got := SupervisorStatusLine(town, 64604, read); got != "none" {
			t.Errorf("SupervisorStatusLine = %q, want %q with no file installed", got, "none")
		}
	})

	t.Run("file does not name a town", func(t *testing.T) {
		path, kind := hostSupervisorPath(t)
		if path == "" {
			t.Skip("no supervisor on this host")
		}
		writeFileAt(t, path, "<plist/>")
		got := SupervisorStatusLine(town, 64604, read)
		if !strings.HasPrefix(got, kind+" (unverified - ") {
			t.Errorf("SupervisorStatusLine = %q, want %q", got, kind+" (unverified - ...)")
		}
	})
}

// hostSupervisorPath resolves the supervisor file this host would use inside a
// throwaway HOME / XDG_DATA_HOME, and fails if the path did not land there.
// The resolved path is a real per-user one, so a test that writes at whatever
// comes back must first have moved HOME into its own temp dir — writing at the
// host's own clobbers the operator's provisioning.
func hostSupervisorPath(t *testing.T) (path, kind string) {
	t.Helper()
	home := t.TempDir()
	dataHome := filepath.Join(home, "data")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", dataHome)
	path, kind = SupervisorFilePath()
	if path == "" {
		return "", ""
	}
	if !strings.HasPrefix(path, home) && !strings.HasPrefix(path, dataHome) {
		t.Fatalf("supervisor file %s is outside the isolated HOME %s / XDG_DATA_HOME %s", path, home, dataHome)
	}
	return path, kind
}

// isolateHome points HOME / XDG_DATA_HOME at throwaway directories without
// resolving anything.
func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
}

// writeHostSupervisorFile installs the plist / unit this host would use, in
// an isolated HOME, naming town as its WorkingDirectory.
func writeHostSupervisorFile(t *testing.T, town string) {
	t.Helper()
	path, kind := hostSupervisorPath(t)
	if path == "" {
		t.Skip("no supervisor on this host")
	}
	var body string
	if kind == "launchd" {
		body = "<plist><dict><key>WorkingDirectory</key>\n<string>" + town + "</string></dict></plist>"
	} else {
		body = "[Service]\nWorkingDirectory=" + town + "\n"
	}
	writeFileAt(t, path, body)
}

func writeFileAt(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubSupervisorProbe answers the next probe with a canned service-manager run.
func stubSupervisorProbe(t *testing.T, out string, err error) {
	t.Helper()
	prev := supervisorProbeRun
	t.Cleanup(func() { supervisorProbeRun = prev })
	supervisorProbeRun = func(argv []string) (string, error) { return out, err }
}

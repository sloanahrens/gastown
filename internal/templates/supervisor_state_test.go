package templates

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	t.Parallel()
	h := probeAnswers(fakeHost(t, "darwin"), "Bad request.\nCould not find service \"com.gastown.daemon\" in domain for user gui: 501\n", errors.New("exit status 113"))

	st := h.jobState("launchd")
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
	t.Parallel()
	h := probeAnswers(fakeHost(t, "darwin"), "", errors.New("exec: \"launchctl\": executable file not found in $PATH"))

	st := h.jobState("launchd")
	if st.Err == nil || st.Loaded {
		t.Errorf("SupervisorJobState(launchd) = %+v, want an error and Loaded=false", st)
	}
}

// The systemd job state takes the unit's invocation id from the same
// 'systemctl show' call that reads the rest of the job, so an owner is matched
// to the run the unit is in (gt-nirga).
func TestSupervisorInvocationID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		out     string
		err     error
		want    string
		wantErr bool
	}{
		{
			name: "the unit reports its invocation",
			out:  "LoadState=loaded\nMainPID=1234\nNRestarts=0\nExecMainStatus=0\nInvocationID=8f3c0a1e2b4d4c6e9a7f1b2c3d4e5f60\n",
			want: "8f3c0a1e2b4d4c6e9a7f1b2c3d4e5f60",
		},
		{
			name: "the unit reports no invocation",
			out:  "LoadState=not-found\nMainPID=0\nNRestarts=0\nExecMainStatus=0\nInvocationID=\n",
		},
		{
			name: "the probe fails", out: "", err: errors.New("exit status 1"), wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := probeAnswers(fakeHost(t, "linux"), tc.out, tc.err).jobState("systemd")
			if st.InvocationID != tc.want {
				t.Errorf("InvocationID = %q, want %q", st.InvocationID, tc.want)
			}
			if (st.Err != nil) != tc.wantErr {
				t.Errorf("Err = %v, want an error: %v", st.Err, tc.wantErr)
			}
		})
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
			"launchd (DETACHED - the job is not loaded, so PID 76426 is not supervised)"},
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
		{"loaded, daemon exited cleanly", SupervisorState{Kind: "launchd", Loaded: true, Runs: 53, LastExit: 0}, 0,
			"launchd (loaded, running no daemon, runs=53, last exit code=0)"},
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

// A container is rendered by its name and what docker says about it, never
// against the lock's PID: that number belongs to the container's namespace,
// so it is nothing to compare here (gt-qpeei.10). An unreadable state stays
// unverified rather than reading as an absent container (gt-qpeei.18).
func TestStatusLine_Container(t *testing.T) {
	t.Parallel()
	const name = "gt-town-daemon-1"
	tests := []struct {
		name    string
		state   SupervisorState
		lockPID int
		want    string
	}{
		{"running", SupervisorState{Kind: "container", Loaded: true, Container: name, Status: "running", Image: "gt-daemon:abc123"},
			4242, "container gt-town-daemon-1 running gt-daemon:abc123"},
		{"exited", SupervisorState{Kind: "container", Loaded: true, Container: name, Status: "exited", Image: "gt-daemon:abc123"},
			0, "container gt-town-daemon-1 exited gt-daemon:abc123"},
		{"restarting", SupervisorState{Kind: "container", Loaded: true, Container: name, Status: "restarting", Image: "gt-daemon:abc123"},
			0, "container gt-town-daemon-1 restarting gt-daemon:abc123"},
		{"absent", SupervisorState{Kind: "container", Container: name}, 4242,
			"container gt-town-daemon-1 (does not exist)"},
		{"unreadable", SupervisorState{Kind: "container", Container: name, Err: errors.New("docker unreachable")}, 4242,
			"container gt-town-daemon-1 (unverified - docker unreachable)"},
		{"no container named", SupervisorState{Kind: "container", Err: errors.New("daemon.container_name and daemon.compose_project: set both or neither")}, 0,
			"container (unverified - daemon.container_name and daemon.compose_project: set both or neither)"},
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
	t.Parallel()
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			other := t.TempDir()
			read := func(kind string) SupervisorState {
				return SupervisorState{Kind: kind, Loaded: true, PID: 64604, Runs: 53, LastExit: 0}
			}

			t.Run("served by this town's supervisor", func(t *testing.T) {
				t.Parallel()
				h := fakeHost(t, goos)
				_, kind := writeSupervisorFile(t, h, town)
				if got := h.statusLine(town, 64604, read); got != kind {
					t.Errorf("statusLine = %q, want the bare kind %q for the daemon the job runs", got, kind)
				}
			})

			t.Run("file of another town", func(t *testing.T) {
				t.Parallel()
				h := fakeHost(t, goos)
				writeSupervisorFile(t, h, other)
				if got := h.statusLine(town, 64604, read); got != "none" {
					t.Errorf("statusLine = %q, want %q for a file naming another town", got, "none")
				}
			})

			t.Run("no file", func(t *testing.T) {
				t.Parallel()
				if got := fakeHost(t, goos).statusLine(town, 64604, read); got != "none" {
					t.Errorf("statusLine = %q, want %q with no file installed", got, "none")
				}
			})

			t.Run("file does not name a town", func(t *testing.T) {
				t.Parallel()
				h := fakeHost(t, goos)
				path, kind := h.filePath()
				writeFileAt(t, path, "<plist/>")
				got := h.statusLine(town, 64604, read)
				if !strings.HasPrefix(got, kind+" (unverified - ") {
					t.Errorf("statusLine = %q, want %q", got, kind+" (unverified - ...)")
				}
			})
		})
	}
}

// A host with no supported supervisor has no file to read and no job.
func TestSupervisorStatusLine_UnsupportedHost(t *testing.T) {
	t.Parallel()
	h := fakeHost(t, "windows")
	if path, kind := h.filePath(); path != "" || kind != "" {
		t.Errorf("filePath() = (%q, %q), want none on an unsupported host", path, kind)
	}
	if got := h.statusLine(t.TempDir(), 0, nil); got != "none" {
		t.Errorf("statusLine = %q, want none", got)
	}
}

// fakeHost is a host running goos whose HOME and XDG_DATA_HOME are fresh
// temp dirs and whose service manager must not be probed.
func fakeHost(t *testing.T, goos string) supervisorHost {
	t.Helper()
	home := t.TempDir()
	dataHome := filepath.Join(home, "data")
	return supervisorHost{
		goos: goos,
		getenv: func(k string) string {
			if k == "XDG_DATA_HOME" {
				return dataHome
			}
			return ""
		},
		homeDir: func() (string, error) { return home, nil },
		probe: func(argv []string) (string, error) {
			t.Errorf("unexpected probe %v", argv)
			return "", errors.New("unexpected probe")
		},
	}
}

// probeAnswers returns h with a service manager that answers every probe with
// a canned run.
func probeAnswers(h supervisorHost, out string, err error) supervisorHost {
	h.probe = func([]string) (string, error) { return out, err }
	return h
}

// writeSupervisorFile installs the plist / unit h would use, naming town as
// its WorkingDirectory, and returns its path and kind.
func writeSupervisorFile(t *testing.T, h supervisorHost, town string) (path, kind string) {
	t.Helper()
	path, kind = h.filePath()
	if path == "" {
		t.Fatalf("host %s has no supervisor file", h.goos)
	}
	var body string
	if kind == "launchd" {
		body = "<plist><dict><key>WorkingDirectory</key>\n<string>" + town + "</string></dict></plist>"
	} else {
		body = "[Service]\nWorkingDirectory=" + town + "\n"
	}
	writeFileAt(t, path, body)
	return path, kind
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

// launchdPrintRespawnLoop is 'launchctl print gui/501/com.gastown.daemon' for
// a job launchd cannot initialize: loaded, counting runs, never a pid, and
// EX_CONFIG as the last exit code (gt-92qx3).
const launchdPrintRespawnLoop = "gui/501/com.gastown.daemon = {\n" +
	"\tactive count = 0\n" +
	"\tstate = spawn scheduled\n" +
	"\truns = 49\n" +
	"\tlast exit code = 78\n"

func TestRespawnLoop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state SupervisorState
		want  bool
	}{
		{"no pid, exit 78", SupervisorState{Kind: "launchd", Loaded: true, Runs: 49, LastExit: LaunchdExitConfig}, true},
		{"running", SupervisorState{Kind: "launchd", Loaded: true, PID: 64604, Runs: 53, LastExit: 0}, false},
		{"no pid, another exit", SupervisorState{Kind: "launchd", Loaded: true, Runs: 58, LastExit: 1}, false},
		{"unreported exit", SupervisorState{Kind: "launchd", Loaded: true, Runs: 58, LastExit: -1}, false},
		{"not loaded", SupervisorState{Kind: "launchd", LastExit: 78}, false},
		{"unreadable", SupervisorState{Kind: "launchd", Loaded: true, LastExit: 78, Err: errors.New("boom")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.state.RespawnLoop(); got != tt.want {
				t.Errorf("RespawnLoop() = %v, want %v for %+v", got, tt.want, tt.state)
			}
		})
	}
}

// The recovery is the one launchd needs and gt never runs. The host's own
// reading of it is pinned first, then the same repair on both host kinds: the
// Linux gate runner replays this reading with no LaunchAgents directory at all
// and must still be handed the repair (gt-92qx3).
func TestSupervisorRepair(t *testing.T) {
	t.Parallel()
	got := SupervisorRepair("launchd")
	if !strings.Contains(got, "launchctl bootout gui/") || !strings.Contains(got, "launchctl bootstrap gui/") {
		t.Errorf("SupervisorRepair(launchd) = %q, want the bootout and bootstrap of the daemon job", got)
	}
	if got := SupervisorRepair("systemd"); got != "" {
		t.Errorf("SupervisorRepair(systemd) = %q, want no recovery named", got)
	}

	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			h := fakeHost(t, goos) // its home holds no plist, and its probe is never called
			home, err := h.homeDir()
			if err != nil {
				t.Fatal(err)
			}
			uid := strconv.Itoa(os.Getuid())
			want := "launchctl bootout gui/" + uid + "/" + LaunchdLabel + " && launchctl bootstrap gui/" + uid +
				" " + filepath.Join(home, "Library", "LaunchAgents", launchdPlistName)
			if got := h.repair("launchd"); got != want {
				t.Errorf("repair(launchd) on %s = %q, want %q", goos, got, want)
			}
			if got := h.repair("systemd"); got != "" {
				t.Errorf("repair(systemd) on %s = %q, want no recovery named", goos, got)
			}
		})
	}
}

// SupervisorRespawn reads the job file before it probes: a job for another
// town's daemon must not be probed, let alone reported as this town's failure.
// The probe itself answers with a canned 'launchctl print'.
func TestSupervisorRespawn(t *testing.T) {
	t.Parallel()
	town, other := t.TempDir(), t.TempDir()

	t.Run("looping job of this town", func(t *testing.T) {
		t.Parallel()
		h := fakeHost(t, "darwin")
		writeSupervisorFile(t, h, town)
		h = probeAnswers(h, launchdPrintRespawnLoop, nil)

		st, looping := h.respawn(town, nil)
		if !looping || st.Runs != 49 || st.LastExit != LaunchdExitConfig {
			t.Errorf("respawn = (%+v, %v), want the loop of this town's job", st, looping)
		}
	})

	t.Run("job of another town is not probed", func(t *testing.T) {
		t.Parallel()
		h := fakeHost(t, "darwin") // its probe fails the test if called
		writeSupervisorFile(t, h, other)

		st, looping := h.respawn(town, nil)
		if looping || st.Err != nil || st.Kind != "launchd" {
			t.Errorf("respawn = (%+v, %v), want the launchd kind, no loop and no error", st, looping)
		}
	})

	t.Run("no job file", func(t *testing.T) {
		t.Parallel()
		h := fakeHost(t, "darwin")

		st, looping := h.respawn(town, nil)
		if looping || st.Err != nil || st.Kind != "launchd" {
			t.Errorf("respawn = (%+v, %v), want the launchd kind, no loop and no error", st, looping)
		}
	})

	t.Run("job file that names no town", func(t *testing.T) {
		t.Parallel()
		h := fakeHost(t, "darwin")
		path, _ := h.filePath()
		writeFileAt(t, path, "<plist/>")

		st, looping := h.respawn(town, nil)
		if looping || st.Err == nil {
			t.Errorf("respawn = (%+v, %v), want an error rather than a verdict", st, looping)
		}
	})

	t.Run("systemd is not this check's job", func(t *testing.T) {
		t.Parallel()
		h := fakeHost(t, "linux")
		writeSupervisorFile(t, h, town)

		st, looping := h.respawn(town, nil)
		if looping || st.Kind != "systemd" {
			t.Errorf("respawn = (%+v, %v), want the systemd kind and no loop", st, looping)
		}
	})
}

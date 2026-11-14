package gtwrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// bdShell is a fake shell for the bddocker target: it answers the ladder's probes
// by the docker verb and whether the ps probe asks for every container or only
// the running ones, records every command line and the environment each attached
// process was given, and never starts docker.
type bdShell struct {
	infoOut     string
	infoExit    int
	infoErr     error
	allOut      string
	allExit     int
	allErr      error
	runningOut  string
	runningExit int
	runningErr  error
	healthOut   string
	healthExit  int
	healthErr   error
	attachExit  int
	attachErr   error

	captured    [][]string
	attached    [][]string
	attachedEnv [][]string
	stdins      []io.Reader
}

func (s *bdShell) Attach(argv []string, env []string) (int, error) {
	return s.AttachStdin(argv, env, nil)
}

func (s *bdShell) AttachStdin(argv []string, env []string, stdin io.Reader) (int, error) {
	s.attached = append(s.attached, argv)
	s.attachedEnv = append(s.attachedEnv, env)
	s.stdins = append(s.stdins, stdin)
	return s.attachExit, s.attachErr
}

func (s *bdShell) AttachStreams(argv []string, env []string, stdin io.Reader, _, _ io.Writer) (int, error) {
	return s.AttachStdin(argv, env, stdin)
}

func (s *bdShell) Capture(_ context.Context, argv []string) (string, int, error) {
	s.captured = append(s.captured, argv)
	switch verbOf(argv) {
	case "info":
		return s.infoOut, s.infoExit, s.infoErr
	case "ps":
		if slices.Contains(argv, "-a") {
			return s.allOut, s.allExit, s.allErr
		}
		return s.runningOut, s.runningExit, s.runningErr
	case "inspect":
		return s.healthOut, s.healthExit, s.healthErr
	}
	panic("unexpected probe: " + strings.Join(argv, " "))
}

// healthyShell answers the ladder as a running, healthy bd container does.
func healthyShell() *bdShell {
	return &bdShell{allOut: "c0ffee\n", runningOut: "c0ffee\n", healthOut: "healthy\n"}
}

// bdDockerTargetFor is a fully wired target for town.
func bdDockerTargetFor(town string, shell Shell) BdDocker {
	return BdDocker{
		Town:      town,
		Container: "gt-bd",
		Caller:    "daemon.test",
		TimeoutMS: 90000,
		Shell:     shell,
		Note:      &bytes.Buffer{},
	}
}

// bdCall is the call a Mac bd invocation plans, through Plan, so the environment
// the target sees is the filtered one.
func bdCall(t *testing.T, town string, env []string) Call {
	t.Helper()
	call, err := Plan("bd", []string{"--allow-stale", "show", "gt-abc", "--json"}, env, filepath.Join(town, "gastown", "crew", "dipper"), false)
	if err != nil {
		t.Fatalf("Plan returned an error: %v", err)
	}
	return call
}

// TestBdDockerTarget drives one whole call through the container target: the
// exec line names the container and `bdgw exec`, the bd arguments after `--`, the
// rig and the town-relative directory on the gateway flags, and the screened
// environment by name (its values stay in the docker process's environment); the
// call's stdin is attached; and the container command's exit status is returned
// unchanged.
func TestBdDockerTarget(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	env := []string{
		"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads"),
		"BD_ACTOR=town/crew/dipper",
		"BD_READONLY=1",
		"GT_TOWN_ROOT=" + town,
		"BEADS_DOLT_SERVER_HOST=elsewhere",
		"BEADS_DOLT_SERVER_PORT=9999",
	}
	for _, tty := range []bool{false, true} {
		t.Run(ttyName(tty), func(t *testing.T) {
			t.Parallel()
			shell := healthyShell()
			shell.attachExit = 7
			call := bdCall(t, town, env)
			call.TTY = tty
			if !tty {
				call.Stdin = strings.NewReader("the stdin body")
			}
			exit, err := bdDockerTargetFor(town, shell).Launch(call)
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if exit != 7 {
				t.Fatalf("exit = %d, want the container command's 7", exit)
			}
			if len(shell.attached) != 1 {
				t.Fatalf("attached %d lines, want 1", len(shell.attached))
			}
			want := []string{"docker", "exec", "-i"}
			if tty {
				want = append(want, "-t")
			}
			want = append(want,
				"-e", "BD_ACTOR",
				"-e", "BD_READONLY",
				"gt-bd", "bdgw", "exec",
				"-rig", "gastown",
				"-cwd", "gastown/crew/dipper",
				"-timeout-ms", "90000",
				"-caller", "daemon.test",
				"--", "--allow-stale", "show", "gt-abc", "--json",
			)
			if !slices.Equal(shell.attached[0], want) {
				t.Fatalf("exec line = %q, want %q", shell.attached[0], want)
			}
			// The values of the forwarded variables travel in the docker process's
			// environment, never in argv.
			for _, entry := range []string{"BD_ACTOR=town/crew/dipper", "BD_READONLY=1"} {
				if slices.Contains(shell.attached[0], entry) {
					t.Errorf("exec line carries the value %q in argv", entry)
				}
				if !slices.Contains(shell.attachedEnv[0], entry) {
					t.Errorf("docker env %q does not carry %q", shell.attachedEnv[0], entry)
				}
			}
			// BEADS_DIR and the Dolt endpoint are the gateway's to compute from the
			// rig, so the caller's values never cross.
			for _, argv := range shell.attached[0] {
				if strings.Contains(argv, ".beads") || strings.Contains(argv, "elsewhere") || strings.Contains(argv, "9999") {
					t.Errorf("exec line carries the caller's %q, which the gateway computes", argv)
				}
			}
			if tty {
				if shell.stdins[0] != nil {
					t.Error("a terminal call was given a stdin reader")
				}
				return
			}
			if shell.stdins[0] == nil {
				t.Fatal("the call's stdin was not attached")
			}
			if got, _ := io.ReadAll(shell.stdins[0]); string(got) != "the stdin body" {
				t.Errorf("stdin = %q, want the call's reader", got)
			}
		})
	}
}

// TestBdDockerTranslation: BEADS_DIR names the rig and the working directory is
// sent relative to the town; a directory outside the town runs in the town root
// with a note; a BEADS_DIR the workspace cannot hold is refused before any
// docker command runs.
func TestBdDockerTranslation(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	inside := filepath.Join(town, "gastown", "crew", "dipper")
	outside := t.TempDir()
	tests := []struct {
		name     string
		env      []string
		dir      string
		wantRig  []string
		wantCwd  string
		wantNote string
		wantErr  string
	}{
		{
			name: "rig and directory",
			env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
			dir:  inside, wantRig: []string{"-rig", "gastown"}, wantCwd: "gastown/crew/dipper",
		},
		{
			name: "town beads",
			env:  []string{"BEADS_DIR=" + filepath.Join(town, ".beads")},
			dir:  filepath.Join(town, "crew", "overseer"), wantCwd: "crew/overseer",
		},
		{
			name: "directory outside the town",
			env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
			dir:  outside, wantRig: []string{"-rig", "gastown"}, wantCwd: ".", wantNote: "outside the town",
		},
		{
			name: "no BEADS_DIR",
			env:  nil, dir: inside, wantErr: "sets no BEADS_DIR",
		},
		{
			name: "relative BEADS_DIR",
			env:  []string{"BEADS_DIR=gastown/.beads"}, dir: inside, wantErr: "not an absolute path",
		},
		{
			name: "BEADS_DIR outside the town",
			env:  []string{"BEADS_DIR=" + filepath.Join(outside, "gastown", ".beads")},
			dir:  inside, wantErr: "outside the town",
		},
		{
			name: "BEADS_DIR that is not a .beads directory",
			env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", "beads")},
			dir:  inside, wantErr: "does not name a .beads directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := healthyShell()
			note := &bytes.Buffer{}
			b := bdDockerTargetFor(town, shell)
			b.Note = note
			exit, err := b.Launch(Call{Argv: []string{"bd", "show", "gt-x"}, Env: tt.env, Dir: tt.dir})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Launch error = %v, want it to name %q", err, tt.wantErr)
				}
				if exit != exitDown {
					t.Errorf("exit = %d, want %d", exit, exitDown)
				}
				if len(shell.captured) != 0 || len(shell.attached) != 0 {
					t.Errorf("a refused call still ran docker: probes %q, calls %q", shell.captured, shell.attached)
				}
				return
			}
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if exit != 0 {
				t.Errorf("exit = %d, want 0", exit)
			}
			if len(shell.attached) != 1 {
				t.Fatalf("attached %d lines, want 1", len(shell.attached))
			}
			argv := shell.attached[0]
			if got := rigFlags(argv); !slices.Equal(got, tt.wantRig) {
				t.Errorf("rig flags = %q, want %q", got, tt.wantRig)
			}
			if got := flagValue(argv, "-cwd"); got != tt.wantCwd {
				t.Errorf("-cwd = %q, want %q", got, tt.wantCwd)
			}
			if tt.wantNote == "" {
				if note.Len() != 0 {
					t.Errorf("note = %q, want none", note)
				}
			} else if !strings.Contains(note.String(), tt.wantNote) {
				t.Errorf("note = %q, want it to name %q", note.String(), tt.wantNote)
			}
		})
	}
}

// rigFlags is the -rig pair of an exec line, or nil when the call names the
// town's own beads.
func rigFlags(argv []string) []string {
	for i, arg := range argv {
		if arg == "-rig" && i+1 < len(argv) {
			return []string{arg, argv[i+1]}
		}
	}
	return nil
}

// flagValue is the value after flag in argv, or "" when flag is absent.
func flagValue(argv []string, flag string) string {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// TestBdDockerErrors: every way a call is refused before docker runs gives one
// line that names what failed, and the probe failures refuse rather than falling
// through to a call.
func TestBdDockerErrors(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	beadsDir := filepath.Join(town, "gastown", ".beads")
	tests := []struct {
		name    string
		mutate  func(*BdDocker, *Call, *bdShell)
		wantErr string
	}{
		{
			name:    "no town root",
			mutate:  func(b *BdDocker, _ *Call, _ *bdShell) { b.Town = "" },
			wantErr: "town root",
		},
		{
			name:    "relative town root",
			mutate:  func(b *BdDocker, _ *Call, _ *bdShell) { b.Town = "gt" },
			wantErr: "must be an absolute path",
		},
		{
			name:    "no container",
			mutate:  func(b *BdDocker, _ *Call, _ *bdShell) { b.Container = "" },
			wantErr: "container is \"\"",
		},
		{
			name:    "a container name that is a flag",
			mutate:  func(b *BdDocker, _ *Call, _ *bdShell) { b.Container = "-rm" },
			wantErr: "not starting with a dash",
		},
		{
			name:    "a container name with a regex metacharacter",
			mutate:  func(b *BdDocker, _ *Call, _ *bdShell) { b.Container = "gt.*" },
			wantErr: "container is",
		},
		{
			name:    "no shell",
			mutate:  func(b *BdDocker, _ *Call, _ *bdShell) { b.Shell = nil },
			wantErr: "no shell",
		},
		{
			name:    "no note writer",
			mutate:  func(b *BdDocker, _ *Call, _ *bdShell) { b.Note = nil },
			wantErr: "no note writer",
		},
		{
			name:    "a call that names no tool",
			mutate:  func(_ *BdDocker, c *Call, _ *bdShell) { c.Argv = nil },
			wantErr: "the call names no tool to run",
		},
		{
			name:    "a call that is not bd",
			mutate:  func(_ *BdDocker, c *Call, _ *bdShell) { c.Argv = []string{"gt", "status"} },
			wantErr: "the bddocker target runs bd",
		},
		{
			name:    "BD_ALLOW_NEW_DATABASE",
			mutate:  func(_ *BdDocker, c *Call, _ *bdShell) { c.Env = append(c.Env, "BD_ALLOW_NEW_DATABASE=1") },
			wantErr: "BD_ALLOW_NEW_DATABASE",
		},
		{
			name:    "docker info fails",
			mutate:  func(_ *BdDocker, _ *Call, s *bdShell) { s.infoExit = 1 },
			wantErr: "Docker is down",
		},
		{
			name:    "docker info cannot run",
			mutate:  func(_ *BdDocker, _ *Call, s *bdShell) { s.infoErr = errors.New("docker: executable not found") },
			wantErr: "Docker is down",
		},
		{
			name:    "ps -a fails",
			mutate:  func(_ *BdDocker, _ *Call, s *bdShell) { s.allExit = 1 },
			wantErr: "could not list the containers",
		},
		{
			name:    "ps -a cannot run",
			mutate:  func(_ *BdDocker, _ *Call, s *bdShell) { s.allErr = errors.New("broken pipe") },
			wantErr: "could not list the containers",
		},
		{
			name:    "ps fails",
			mutate:  func(_ *BdDocker, _ *Call, s *bdShell) { s.runningExit = 1 },
			wantErr: "is running",
		},
		{
			name:    "ps cannot run",
			mutate:  func(_ *BdDocker, _ *Call, s *bdShell) { s.runningErr = errors.New("broken pipe") },
			wantErr: "is running",
		},
		{
			name:    "inspect fails",
			mutate:  func(_ *BdDocker, _ *Call, s *bdShell) { s.healthExit = 1 },
			wantErr: "health of the bd container",
		},
		{
			name:    "inspect cannot run",
			mutate:  func(_ *BdDocker, _ *Call, s *bdShell) { s.healthErr = errors.New("broken pipe") },
			wantErr: "health of the bd container",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := healthyShell()
			b := bdDockerTargetFor(town, shell)
			call := Call{Argv: []string{"bd", "show", "gt-x"}, Env: []string{"BEADS_DIR=" + beadsDir}, Dir: town}
			tt.mutate(&b, &call, shell)
			exit, err := b.Launch(call)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Launch error = %v, want it to name %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("the error is not one line: %q", err)
			}
			if exit != exitDown {
				t.Errorf("exit = %d, want %d", exit, exitDown)
			}
			if len(shell.attached) != 0 {
				t.Errorf("a refused call still ran in the container: %q", shell.attached)
			}
		})
	}
}

// TestBdDockerDoctor: the doctor names the first step of the ladder that fails
// and reports the container a call would reach; it runs no call in the container
// and there is no throwaway rung, so a missing, stopped or unhealthy container is
// down.
func TestBdDockerDoctor(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tests := []struct {
		name    string
		mutate  func(*bdShell)
		want    Rung
		wantErr []string
	}{
		{
			name: "healthy",
			want: RungExec, wantErr: []string{"is running and healthy"},
		},
		{
			name:    "docker down",
			mutate:  func(s *bdShell) { s.infoExit = 1 },
			want:    RungDown,
			wantErr: []string{"Docker is down", "gt dolt start"},
		},
		{
			name:    "container missing",
			mutate:  func(s *bdShell) { s.allOut = "\n" },
			want:    RungDown,
			wantErr: []string{"does not exist", "gt dolt start"},
		},
		{
			name:    "container stopped",
			mutate:  func(s *bdShell) { s.runningOut = "\n" },
			want:    RungDown,
			wantErr: []string{"is stopped", "gt dolt start"},
		},
		{
			name:    "container unhealthy",
			mutate:  func(s *bdShell) { s.healthOut = "unhealthy\n" },
			want:    RungDown,
			wantErr: []string{"is unhealthy", "bdgw ready"},
		},
		{
			name:    "container starting",
			mutate:  func(s *bdShell) { s.healthOut = "starting\n" },
			want:    RungDown,
			wantErr: []string{"is starting"},
		},
		{
			name:    "no health answer",
			mutate:  func(s *bdShell) { s.healthOut = "\n" },
			want:    RungDown,
			wantErr: []string{"reports health \"\""},
		},
		{
			name:    "health cannot be read",
			mutate:  func(s *bdShell) { s.healthErr = errors.New("no such object") },
			want:    RungDown,
			wantErr: []string{"health of the bd container", "bdgw ready"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := healthyShell()
			if tt.mutate != nil {
				tt.mutate(shell)
			}
			diag, err := bdDockerTargetFor(town, shell).Doctor()
			if err != nil {
				t.Fatalf("Doctor: %v", err)
			}
			if diag.Rung != tt.want {
				t.Errorf("rung = %s, want %s (reason: %s)", diag.Rung, tt.want, diag.Reason)
			}
			if diag.Container != "gt-bd" {
				t.Errorf("container = %q, want gt-bd", diag.Container)
			}
			for _, part := range tt.wantErr {
				if !strings.Contains(diag.Reason, part) {
					t.Errorf("reason %q does not name %q", diag.Reason, part)
				}
			}
			if len(shell.attached) != 0 {
				t.Errorf("the doctor ran a call in the container: %q", shell.attached)
			}
		})
	}
}

// TestBdDockerDefaults: an unset limit and caller fall back to the design's
// largest class cap and the default caller name, so the gateway always gets both.
func TestBdDockerDefaults(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	shell := healthyShell()
	b := bdDockerTargetFor(town, shell)
	b.TimeoutMS, b.Caller = 0, ""
	call := Call{Argv: []string{"bd", "show", "gt-x"}, Env: []string{"BEADS_DIR=" + filepath.Join(town, ".beads")}, Dir: town}
	if _, err := b.Launch(call); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := flagValue(shell.attached[0], "-timeout-ms"); got != "120000" {
		t.Errorf("-timeout-ms = %q, want the 120000 default", got)
	}
	if got := flagValue(shell.attached[0], "-caller"); got != "gt-wrap" {
		t.Errorf("-caller = %q, want the gt-wrap default", got)
	}
}

// TestBdDockerReturnsTheShellError: a docker command that cannot start is the
// shell's error and status, passed through rather than read as bd's answer.
func TestBdDockerReturnsTheShellError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	shell := healthyShell()
	shell.attachErr = errors.New("docker: executable not found")
	shell.attachExit = exitDown
	b := bdDockerTargetFor(town, shell)
	call := Call{Argv: []string{"bd", "show", "gt-x"}, Env: []string{"BEADS_DIR=" + filepath.Join(town, ".beads")}, Dir: town}
	exit, err := b.Launch(call)
	if err == nil || !strings.Contains(err.Error(), "docker: executable not found") {
		t.Fatalf("Launch error = %v, want the shell's error", err)
	}
	if exit != exitDown {
		t.Errorf("exit = %d, want %d", exit, exitDown)
	}
}

// TestBdDockerDoctorRefusesAnInvalidTarget: a target the doctor cannot use is an
// error naming the field, not a down diagnosis, and no probe runs.
func TestBdDockerDoctorRefusesAnInvalidTarget(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tests := []struct {
		name    string
		mutate  func(*BdDocker)
		wantErr string
	}{
		{"no town root", func(b *BdDocker) { b.Town = "" }, "town root"},
		{"relative town root", func(b *BdDocker) { b.Town = "gt" }, "must be an absolute path"},
		{"no container", func(b *BdDocker) { b.Container = "" }, "container is \"\""},
		{"no shell", func(b *BdDocker) { b.Shell = nil }, "no shell"},
		{"no note writer", func(b *BdDocker) { b.Note = nil }, "no note writer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := healthyShell()
			b := bdDockerTargetFor(town, shell)
			tt.mutate(&b)
			if _, err := b.Doctor(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Doctor error = %v, want it to name %q", err, tt.wantErr)
			}
			if len(shell.captured) != 0 {
				t.Errorf("an invalid target ran docker probes: %q", shell.captured)
			}
		})
	}
}

// TestBdDockerLaunchHasNoThrowawayRung: when the container is missing, stopped
// or unhealthy, a call is refused with the doctor's reason and nothing runs in a
// throwaway container.
func TestBdDockerLaunchHasNoThrowawayRung(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tests := []struct {
		name   string
		mutate func(*bdShell)
		want   string
	}{
		{"missing", func(s *bdShell) { s.allOut = "" }, "does not exist"},
		{"stopped", func(s *bdShell) { s.runningOut = "" }, "is stopped"},
		{"unhealthy", func(s *bdShell) { s.healthOut = "unhealthy" }, "is unhealthy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := healthyShell()
			tt.mutate(shell)
			b := bdDockerTargetFor(town, shell)
			call := Call{Argv: []string{"bd", "show", "gt-x"}, Env: []string{"BEADS_DIR=" + filepath.Join(town, ".beads")}, Dir: town}
			exit, err := b.Launch(call)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Launch error = %v, want it to name %q", err, tt.want)
			}
			if exit != exitDown {
				t.Errorf("exit = %d, want %d", exit, exitDown)
			}
			if len(shell.attached) != 0 {
				t.Errorf("a call ran in a throwaway container: %q", shell.attached)
			}
		})
	}
}

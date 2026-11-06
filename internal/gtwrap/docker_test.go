package gtwrap

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/templates"
)

const (
	testTown = "/Users/operator/gt"
	testDir  = "/Users/operator/gt/rig/polecats"
	// testImage is not the image the wrapper would name by itself, so a test that
	// passes proves the tag came from the compose file.
	testImage = "gt-daemon:7f3e9a1"
	// testDolt is the Dolt host every planned command line carries.
	testDolt = "BEADS_DOLT_SERVER_HOST=host.docker.internal"
)

var testComposePath = filepath.Join(testTown, "daemon", "container", "compose.yml")

const testCompose = `services:
  daemon:
    image: "gt-daemon:7f3e9a1"
    container_name: "gt-daemon"
  gt-dolt:
    image: "dolt:1"
`

// fakeShell records every docker line and answers from canned fields. Capture
// answers by the docker verb: info and ps are the only probes Launch makes.
type fakeShell struct {
	infoExit int
	infoErr  error
	psOut    string
	psExit   int
	psErr    error
	// toolOut, toolExit and toolErr answer ToolExists's check of a tool in the
	// image.
	toolOut    string
	toolExit   int
	toolErr    error
	attachExit int
	attachErr  error
	// outlast makes each bounded probe run until its context stops it, as a hung
	// engine does. A stopped command exits -1, which OSShell reports the same way.
	outlast bool

	captured [][]string
	// unbounded lists the probes that ran with no deadline.
	unbounded [][]string
	attached  [][]string
}

func (f *fakeShell) Attach(argv []string) (int, error) {
	f.attached = append(f.attached, argv)
	return f.attachExit, f.attachErr
}

func (f *fakeShell) Capture(ctx context.Context, argv []string) (string, int, error) {
	f.captured = append(f.captured, argv)
	_, bounded := ctx.Deadline()
	if !bounded {
		f.unbounded = append(f.unbounded, argv)
	}
	if f.outlast && bounded {
		<-ctx.Done()
		return "", -1, nil
	}
	switch verbOf(argv) {
	case "info":
		return "", f.infoExit, f.infoErr
	case "ps":
		return f.psOut, f.psExit, f.psErr
	case "exec", "run":
		return f.toolOut, f.toolExit, f.toolErr
	}
	panic("unexpected probe: " + strings.Join(argv, " "))
}

// verbOf is the docker subcommand of argv, past any --context flag.
func verbOf(argv []string) string {
	args := argv[1:]
	if len(args) >= 2 && args[0] == "--context" {
		args = args[2:]
	}
	return args[0]
}

// readCompose serves the compose file at the town's path and nothing else, so
// a test that builds the wrong path fails to find the file.
func readCompose(data string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if path != testComposePath {
			return nil, fs.ErrNotExist
		}
		return []byte(data), nil
	}
}

// testHMConfig is the derived hm config a throwaway hm run binds, under the town.
var testHMConfig = filepath.Join(testTown, filepath.FromSlash(hmConfigTownPath))

// fakeStat answers a stat from a table: a listed path is an entry of that mode,
// and an unlisted path does not exist.
func fakeStat(modes map[string]fs.FileMode) func(string) (fs.FileInfo, error) {
	return func(path string) (fs.FileInfo, error) {
		mode, ok := modes[path]
		if !ok {
			return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
		}
		return fakeInfo{name: filepath.Base(path), mode: mode}, nil
	}
}

// ttyName names a subtest by whether stdin is a terminal.
func ttyName(tty bool) string {
	if tty {
		return "stdin-is-a-terminal"
	}
	return "stdin-is-not-a-terminal"
}

// testCall builds the call the wrapper makes for `gt status --line`, through Plan,
// so the environment the runner sees is the filtered one.
func testCall(t *testing.T, env []string, tty bool) Call {
	t.Helper()
	call, err := Plan("gt", []string{"status", "--line"}, env, testDir, tty)
	if err != nil {
		t.Fatalf("Plan returned an error: %v", err)
	}
	return call
}

var callEnv = []string{
	"HOME=/Users/operator",
	"AWS_SECRET_ACCESS_KEY=secret",
	"GT_TOWN=" + testTown,
	"TERM=xterm-256color",
}

func TestLadderExecsIntoTheRunningContainer(t *testing.T) {
	t.Parallel()
	for _, tty := range []bool{false, true} {
		t.Run(ttyName(tty), func(t *testing.T) {
			t.Parallel()
			shell := &fakeShell{psOut: "5c1e0a7f\n"}
			d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
			exit, err := d.Launch(testCall(t, callEnv, tty))
			if err != nil || exit != 0 {
				t.Fatalf("Launch = %d, %v; want 0, nil", exit, err)
			}
			want := []string{"docker", "exec", "-i"}
			if tty {
				want = append(want, "-t")
			}
			want = append(want,
				"-w", testDir,
				"-e", "GT_TOWN="+testTown,
				"-e", "TERM=xterm-256color",
				"-e", testDolt,
				"gt-daemon", "gt", "status", "--line",
			)
			if len(shell.attached) != 1 || !slices.Equal(shell.attached[0], want) {
				t.Fatalf("attached %q, want exactly one exec line %q", shell.attached, want)
			}
		})
	}
}

func TestLadderRunsAThrowawayContainerWhenTheDaemonIsStopped(t *testing.T) {
	t.Parallel()
	for _, tty := range []bool{false, true} {
		t.Run(ttyName(tty), func(t *testing.T) {
			t.Parallel()
			shell := &fakeShell{psOut: ""}
			d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
			exit, err := d.Launch(testCall(t, callEnv, tty))
			if err != nil || exit != 0 {
				t.Fatalf("Launch = %d, %v; want 0, nil", exit, err)
			}
			want := []string{"docker", "run", "--rm", "-i"}
			if tty {
				want = append(want, "-t")
			}
			want = append(want,
				"-w", testDir,
				"-e", "GT_TOWN="+testTown,
				"-e", "TERM=xterm-256color",
				"-e", testDolt,
				"-v", testTown+":"+testTown,
				"--entrypoint", "gt",
				testImage, "status", "--line",
			)
			if len(shell.attached) != 1 || !slices.Equal(shell.attached[0], want) {
				t.Fatalf("attached %q, want exactly one run line %q", shell.attached, want)
			}
		})
	}
}

func TestLadderReportsDockerDownWithTheStartCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		infoExit int
		infoErr  error
	}{
		{"info exits non-zero", 1, nil},
		{"info cannot run", 0, errors.New("docker: executable not found")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := &fakeShell{infoExit: tt.infoExit, infoErr: tt.infoErr}
			d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
			exit, err := d.Launch(testCall(t, callEnv, false))
			if err == nil || exit == 0 {
				t.Fatalf("Launch = %d, %v; want a non-zero exit and an error", exit, err)
			}
			for _, want := range []string{
				"Docker Desktop",
				"gt-daemon",
				"open -a Docker",
				"compose -p gt-live -f " + testComposePath + " up -d daemon",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
			if len(shell.attached) != 0 {
				t.Fatalf("Docker is down but the call was attached: %q", shell.attached)
			}
		})
	}
}

func TestLadderRefusesWhenTheContainerStateIsUnknown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		shell fakeShell
	}{
		{"ps exits non-zero", fakeShell{psExit: 1}},
		{"ps cannot run", fakeShell{psErr: errors.New("docker: executable not found")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := tt.shell
			d := Docker{Town: testTown, Shell: &shell, ReadFile: readCompose(testCompose)}
			exit, err := d.Launch(testCall(t, callEnv, false))
			if err == nil || exit == 0 {
				t.Fatalf("Launch = %d, %v; want a refusal", exit, err)
			}
			if len(shell.attached) != 0 {
				t.Fatalf("container state is unknown but the call was attached: %q", shell.attached)
			}
		})
	}
}

func TestLadderRefusesWhenTheComposeFileIsUnusable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		town string
		read func(string) ([]byte, error)
	}{
		{"file is missing", testTown, func(string) ([]byte, error) { return nil, fs.ErrNotExist }},
		{"file does not parse", testTown, readCompose("services: [\n")},
		{"daemon names no image", testTown, readCompose("services:\n  daemon:\n    container_name: \"gt-daemon\"\n")},
		{"daemon names no container", testTown, readCompose("services:\n  daemon:\n    image: \"gt-daemon:7f3e9a1\"\n")},
		// The reader takes any path, so only the empty-town check can refuse this row.
		{"no town root", "", func(string) ([]byte, error) { return []byte(testCompose), nil }},
	}
	// Each rung reads the compose file itself, so each refuses on its own.
	runs := []struct {
		name string
		run  func(Docker, Call) (int, error)
	}{{"launch", Docker.Launch}, {"exec", Docker.Exec}, {"run", Docker.Run}}
	for _, tt := range tests {
		for _, r := range runs {
			t.Run(tt.name+"/"+r.name, func(t *testing.T) {
				t.Parallel()
				shell := &fakeShell{psOut: "5c1e0a7f\n"}
				d := Docker{Town: tt.town, Shell: shell, ReadFile: tt.read}
				exit, err := r.run(d, testCall(t, callEnv, false))
				if err == nil || exit == 0 {
					t.Fatalf("%s = %d, %v; want a refusal", r.name, exit, err)
				}
				if len(shell.captured) != 0 || len(shell.attached) != 0 {
					t.Fatalf("compose file is unusable but docker ran: probes %q, calls %q", shell.captured, shell.attached)
				}
			})
		}
	}
}

func TestProbeResultReturnsTheStdoutOfAProbeThatRan(t *testing.T) {
	t.Parallel()
	out, exit, err := probeResult([]byte("5c1e0a7f\n"), nil)
	if err != nil || exit != 0 || out != "5c1e0a7f\n" {
		t.Fatalf("probeResult = %q, %d, %v; want the stdout, 0, nil", out, exit, err)
	}
}

func TestProbeResultReportsAProbeThatDidNotRun(t *testing.T) {
	t.Parallel()
	out, exit, err := probeResult(nil, errors.New("docker: executable not found"))
	if err == nil || exit == 0 {
		t.Fatalf("probeResult = %q, %d, %v; want an error and a non-zero exit", out, exit, err)
	}
}

func TestLadderBoundsEveryProbe(t *testing.T) {
	t.Parallel()
	shell := &fakeShell{psOut: "5c1e0a7f\n"}
	d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
	if _, err := d.Launch(testCall(t, callEnv, false)); err != nil {
		t.Fatalf("Launch returned an error: %v", err)
	}
	if len(shell.captured) != 2 {
		t.Fatalf("Launch made %d probes, want info and ps", len(shell.captured))
	}
	if len(shell.unbounded) != 0 {
		t.Fatalf("probes ran with no deadline, so a hung engine would hang the caller: %q", shell.unbounded)
	}
}

func TestLadderRefusesWhenAProbeRunsOutOfTime(t *testing.T) {
	t.Parallel()
	shell := &fakeShell{outlast: true}
	d := Docker{Town: testTown, ProbeTimeout: time.Nanosecond, Shell: shell, ReadFile: readCompose(testCompose)}
	exit, err := d.Launch(testCall(t, callEnv, false))
	if err == nil || exit == 0 {
		t.Fatalf("Launch = %d, %v; want a refusal when the engine does not answer", exit, err)
	}
	if len(shell.attached) != 0 {
		t.Fatalf("the engine did not answer but the call was attached: %q", shell.attached)
	}
}

func TestLadderRefusesACallWithNoTool(t *testing.T) {
	t.Parallel()
	for _, run := range []func(Docker, Call) (int, error){Docker.Launch, Docker.Exec, Docker.Run} {
		shell := &fakeShell{psOut: "5c1e0a7f\n"}
		d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
		exit, err := run(d, Call{Dir: testDir})
		if err == nil || exit == 0 {
			t.Fatalf("run = %d, %v; want a refusal for a call with no tool", exit, err)
		}
		if len(shell.captured) != 0 || len(shell.attached) != 0 {
			t.Fatalf("call has no tool but docker ran: probes %q, calls %q", shell.captured, shell.attached)
		}
	}
}

func TestLadderReturnsTheExitCodeUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		psOut string
	}{
		{"exec rung", "5c1e0a7f\n"},
		{"run rung", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := &fakeShell{psOut: tt.psOut, attachExit: 7}
			d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
			exit, err := d.Launch(testCall(t, callEnv, false))
			if err != nil {
				t.Fatalf("Launch returned an error for a tool that ran: %v", err)
			}
			if exit != 7 {
				t.Fatalf("Launch = %d, want the tool's own status 7", exit)
			}
		})
	}
}

func TestLadderRunsEveryCommandInTheNamedContext(t *testing.T) {
	t.Parallel()
	for _, psOut := range []string{"5c1e0a7f\n", ""} {
		shell := &fakeShell{psOut: psOut}
		d := Docker{Town: testTown, Context: "desktop-linux", Shell: shell, ReadFile: readCompose(testCompose)}
		if _, err := d.Launch(testCall(t, callEnv, false)); err != nil {
			t.Fatalf("Launch returned an error: %v", err)
		}
		prefix := []string{"docker", "--context", "desktop-linux"}
		for _, argv := range append(append([][]string{}, shell.captured...), shell.attached...) {
			if !slices.Equal(argv[:len(prefix)], prefix) {
				t.Fatalf("command %q does not run in the named context", argv)
			}
		}
	}
}

func TestRunnerSetsTheDoltHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  []string
	}{
		{"caller forwards the container's own loopback", []string{"BEADS_DOLT_SERVER_HOST=127.0.0.1", "GT_TOWN=" + testTown}},
		{"caller forwards nothing", []string{"GT_TOWN=" + testTown}},
	}
	rungs := []struct {
		name string
		run  func(Docker, Call) (int, error)
	}{
		{"exec", Docker.Exec},
		{"run", Docker.Run},
	}
	for _, tt := range tests {
		for _, rung := range rungs {
			t.Run(tt.name+"/"+rung.name, func(t *testing.T) {
				t.Parallel()
				shell := &fakeShell{}
				d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
				if _, err := rung.run(d, testCall(t, tt.env, false)); err != nil {
					t.Fatalf("%s returned an error: %v", rung.name, err)
				}
				if len(shell.attached) != 1 {
					t.Fatalf("%s attached %d lines, want 1", rung.name, len(shell.attached))
				}
				argv := shell.attached[0]
				if !slices.Contains(argv, testDolt) {
					t.Fatalf("%s line %q does not set %s to the host gateway", rung.name, argv, testDolt)
				}
				var dolt int
				for _, arg := range argv {
					if strings.HasPrefix(arg, "BEADS_DOLT_SERVER_HOST=") {
						dolt++
					}
				}
				if dolt != 1 {
					t.Fatalf("%s line %q sets BEADS_DOLT_SERVER_HOST %d times, want once", rung.name, argv, dolt)
				}
			})
		}
	}
}

func TestToolExistsChecksTheRunningContainer(t *testing.T) {
	t.Parallel()
	shell := &fakeShell{psOut: "5c1e0a7f\n", toolOut: "/usr/local/bin/gt\n"}
	d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
	if err := d.ToolExists("gt"); err != nil {
		t.Fatalf("ToolExists(gt) = %v, want nil", err)
	}
	want := []string{"docker", "exec", "gt-daemon", "sh", "-c", toolCheck, "sh", "gt"}
	if len(shell.captured) != 3 || !slices.Equal(shell.captured[2], want) {
		t.Fatalf("probes = %q, want the info and ps probes then %q", shell.captured, want)
	}
}

func TestToolExistsChecksAThrowawayContainerWhenTheDaemonIsStopped(t *testing.T) {
	t.Parallel()
	shell := &fakeShell{psOut: "", toolOut: "/usr/local/bin/om\n"}
	d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
	if err := d.ToolExists("om"); err != nil {
		t.Fatalf("ToolExists(om) = %v, want nil", err)
	}
	want := []string{"docker", "run", "--rm", "--entrypoint", "sh", testImage, "-c", toolCheck, "sh", "om"}
	if len(shell.captured) != 3 || !slices.Equal(shell.captured[2], want) {
		t.Fatalf("probes = %q, want the info and ps probes then %q", shell.captured, want)
	}
}

func TestToolExistsRefusesAToolThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		shell fakeShell
	}{
		{"the check exits non-zero", fakeShell{psOut: "5c1e0a7f\n", toolExit: 127}},
		{"the check finds nothing", fakeShell{psOut: "5c1e0a7f\n"}},
		{"the check cannot run", fakeShell{psOut: "5c1e0a7f\n", toolErr: errors.New("docker: executable not found")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := tt.shell
			d := Docker{Town: testTown, Shell: &shell, ReadFile: readCompose(testCompose)}
			err := d.ToolExists("hm")
			if err == nil {
				t.Fatal("ToolExists(hm) = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), "hm") || !strings.Contains(err.Error(), testImage) {
				t.Fatalf("error %q does not name the tool and the image", err)
			}
		})
	}
}

func TestToolExistsRefusesWhenTheRungIsDown(t *testing.T) {
	t.Parallel()
	shell := &fakeShell{infoExit: 1}
	d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
	err := d.ToolExists("gt")
	if err == nil {
		t.Fatal("ToolExists = nil with Docker down, want a refusal")
	}
	if !strings.Contains(err.Error(), "Docker Desktop") {
		t.Fatalf("error %q does not say what is down", err)
	}
	if len(shell.captured) != 1 {
		t.Fatalf("probes = %q, want only the info probe", shell.captured)
	}
}

func TestToolExistsRefusesANameItDoesNotAnswerAs(t *testing.T) {
	t.Parallel()
	shell := &fakeShell{}
	d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
	if err := d.ToolExists("bash"); err == nil {
		t.Fatal("ToolExists(bash) = nil, want a refusal")
	}
	if len(shell.captured) != 0 {
		t.Fatalf("a name the wrapper does not answer as ran docker: %q", shell.captured)
	}
}

func TestToolExistsRefusesAnUnusableComposeFile(t *testing.T) {
	t.Parallel()
	shell := &fakeShell{}
	d := Docker{Town: testTown, Shell: shell, ReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist }}
	if err := d.ToolExists("gt"); err == nil {
		t.Fatal("ToolExists = nil with no compose file, want a refusal")
	}
	if len(shell.captured) != 0 {
		t.Fatalf("the compose file is unusable but docker ran: %q", shell.captured)
	}
}

// TestRunnerSetsHMConfig: every hm call carries HM_CONFIG set to the mounted
// config's path in the container, replacing a forwarded value; gt, bd and om
// carry none, and rung 2 mounts the derived file where that path names it.
func TestRunnerSetsHMConfig(t *testing.T) {
	t.Parallel()
	tools := []struct {
		name string
		argv []string
		hm   bool
	}{
		{"hm", []string{"hm", "status"}, true},
		{"gt", []string{"gt", "status", "--line"}, false},
		{"bd", []string{"bd", "show", "gt-abc"}, false},
		{"om", []string{"om", "review"}, false},
	}
	envs := []struct {
		name string
		env  []string
	}{
		{"nothing forwarded", []string{"GT_TOWN=" + testTown}},
		{"a forwarded HM_CONFIG is replaced", []string{"HM_CONFIG=/Users/operator/.config/gt/hm-config.json", "GT_TOWN=" + testTown}},
	}
	rungs := []struct {
		name string
		run  func(Docker, Call) (int, error)
	}{
		{"exec", Docker.Exec},
		{"run", Docker.Run},
	}
	for _, tool := range tools {
		for _, env := range envs {
			for _, rung := range rungs {
				t.Run(tool.name+"/"+env.name+"/"+rung.name, func(t *testing.T) {
					t.Parallel()
					shell := &fakeShell{}
					d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose), Stat: fakeStat(map[string]fs.FileMode{testHMConfig: 0o600})}
					call := Call{Argv: tool.argv, Env: env.env, Dir: testDir}
					if _, err := rung.run(d, call); err != nil {
						t.Fatalf("%s returned an error: %v", rung.name, err)
					}
					if len(shell.attached) != 1 {
						t.Fatalf("%s attached %d lines, want 1", rung.name, len(shell.attached))
					}
					argv := shell.attached[0]
					var set []string
					for _, arg := range argv {
						if strings.HasPrefix(arg, hmConfigVar+"=") {
							set = append(set, arg)
						}
					}
					if !tool.hm {
						if len(set) != 0 {
							t.Fatalf("%s line %q sets %s, want none for %s", rung.name, argv, hmConfigVar, tool.name)
						}
						return
					}
					if want := []string{hmConfigVar + "=" + hmConfigPath}; !slices.Equal(set, want) {
						t.Fatalf("%s line %q sets %s %q, want exactly %q", rung.name, argv, hmConfigVar, set, want)
					}
					for _, arg := range argv {
						if strings.Contains(arg, "/Users/operator/.config/gt/hm-config.json") {
							t.Fatalf("%s line %q carries the caller's own %s", rung.name, argv, hmConfigVar)
						}
					}
					wantMount := filepath.Join(testTown, filepath.FromSlash(hmConfigTownPath)) + ":" + hmConfigPath + ":ro"
					switch rung.name {
					case "run":
						if !slices.Contains(argv, wantMount) {
							t.Fatalf("%s line %q does not mount the derived config %s", rung.name, argv, wantMount)
						}
					case "exec":
						if slices.Contains(argv, wantMount) {
							t.Fatalf("%s line %q mounts the config itself, but the running container carries it", rung.name, argv)
						}
					}
				})
			}
		}
	}
}

// TestHMConfigPathIsTheRenderTarget pins the wrapper's HM_CONFIG path to the
// render's mount target, so the two cannot send hm to different files.
func TestHMConfigPathIsTheRenderTarget(t *testing.T) {
	t.Parallel()
	if hmConfigPath != templates.LiveHMConfigPath {
		t.Fatalf("the wrapper names %s, the render mounts %s", hmConfigPath, templates.LiveHMConfigPath)
	}
}

// TestHMConfigTownPathIsTheWriteTarget pins the throwaway run's source path to the
// path the compose write derives, so the two cannot read different files.
func TestHMConfigTownPathIsTheWriteTarget(t *testing.T) {
	t.Parallel()
	if hmConfigTownPath != templates.ContainerHMConfigRP {
		t.Fatalf("the wrapper binds %s, the compose write derives %s", hmConfigTownPath, templates.ContainerHMConfigRP)
	}
}

// TestRunnerRefusesAMissingHMConfig: a throwaway hm run is refused, naming the
// derived config and the command that generates it, when that path is absent, a
// directory, unreadable, or never checked, and it attaches no engine command. A
// regular file plans as before. Launch drives the run rung: the container is
// stopped, so the call would otherwise be a throwaway run.
func TestRunnerRefusesAMissingHMConfig(t *testing.T) {
	t.Parallel()
	hmCall := Call{Argv: []string{"hm", "status"}, Env: []string{"GT_TOWN=" + testTown}, Dir: testDir}
	refusals := []struct {
		name string
		stat func(string) (fs.FileInfo, error)
	}{
		{"absent", fakeStat(nil)},
		{"a directory", fakeStat(map[string]fs.FileMode{testHMConfig: fs.ModeDir | 0o755})},
		{"unreadable", func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }},
		{"never checked", nil},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := &fakeShell{}
			d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose), Stat: tt.stat}
			_, err := d.Launch(hmCall)
			if err == nil {
				t.Fatalf("Launch planned a throwaway hm run with %s, want a refusal", tt.name)
			}
			for _, want := range []string{testHMConfig, "gt daemon compose write"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %q", err, want)
				}
			}
			if len(shell.attached) != 0 {
				t.Fatalf("attached %q, want no engine command", shell.attached)
			}
		})
	}
	t.Run("a regular file plans as before", func(t *testing.T) {
		t.Parallel()
		shell := &fakeShell{}
		d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose), Stat: fakeStat(map[string]fs.FileMode{testHMConfig: 0o600})}
		if _, err := d.Launch(hmCall); err != nil {
			t.Fatalf("Launch returned an error for a regular file: %v", err)
		}
		if len(shell.attached) != 1 {
			t.Fatalf("attached %d lines, want 1", len(shell.attached))
		}
		want := testHMConfig + ":" + hmConfigPath + ":ro"
		if !slices.Contains(shell.attached[0], want) {
			t.Fatalf("line %q does not mount the derived config %s", shell.attached[0], want)
		}
	})
}

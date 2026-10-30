package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/templates"
)

// daemonScratchRig is a live town, a scratch root holding its marker and a compose
// file path, all real directories under one temp dir, with a fake runner that
// records what it was given and what the compose file held when it ran.
type daemonScratchRig struct {
	flags scratchFlags
	deps  scratchDeps
	out   *bytes.Buffer
	runs  []daemonScratchRun
}

type daemonScratchRun struct {
	dir     string
	env     []string
	argv    []string
	compose string // the compose file's content when the command ran, "" if absent
}

func newDaemonScratchRig(t *testing.T) *daemonScratchRig {
	t.Helper()
	root := t.TempDir()
	live := filepath.Join(root, "live")
	if err := os.MkdirAll(filepath.Join(live, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "mayor", "town.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "scratch", "town")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, config.ScratchTownMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &daemonScratchRig{out: &bytes.Buffer{}}
	r.flags = scratchFlags{
		liveRoot:    live,
		root:        scratch,
		composeFile: filepath.Join(root, "scratch", "compose.yml"),
		image:       "gt-daemon:0123456",
		project:     "gt-scratch",
		container:   "gt-scratch-daemon-1",
	}
	r.deps = scratchDeps{
		run: func(_ context.Context, dir string, env, argv []string) error {
			content, _ := os.ReadFile(r.flags.composeFile)
			r.runs = append(r.runs, daemonScratchRun{dir: dir, env: env, argv: argv, compose: string(content)})
			return nil
		},
		environ: []string{"PATH=/usr/bin", "HOME=/home/x", "DOCKER_CONTEXT=other", "DOCKER_HOST=tcp://elsewhere:2375"},
		out:     r.out,
		stat:    os.Stat,
		lstat:   os.Lstat,

		readFile:     os.ReadFile,
		readPatrol:   daemon.ReadPatrolConfig,
		evalSymlinks: filepath.EvalSymlinks,
	}
	return r
}

func (r *daemonScratchRig) spec() templates.ComposeSpec {
	return templates.ComposeSpec{
		Project: r.flags.project, ContainerName: r.flags.container, Image: r.flags.image,
		TownRoot: r.flags.root, StopGrace: templates.MinimumStopGrace, Scratch: true,
		ScratchDoltImage: templates.DoltServerImage, LiveTownRoot: r.flags.liveRoot,
		OMTokenFile: r.flags.omTokenFile,
	}
}

// forgeSpec is the same spec with the throwaway forge on, the way both port
// flags ask for it.
func (r *daemonScratchRig) forgeSpec(httpPort, sshPort int) templates.ComposeSpec {
	s := r.spec()
	s.ScratchForge, s.ScratchForgeHTTPPort, s.ScratchForgeSSHPort = true, httpPort, sshPort
	return s
}

// setForge gives both forge port flags on the rig's flags.
func (r *daemonScratchRig) setForge(httpPort, sshPort int) {
	r.flags.forge = forgePorts{httpPort: httpPort, sshPort: sshPort, httpSet: true, sshSet: true}
}

// composeExists reports whether the compose file is a regular file on disk.
func (r *daemonScratchRig) composeExists() bool {
	info, err := os.Stat(r.flags.composeFile)
	return err == nil && info.Mode().IsRegular()
}

// render prints exactly what the scratch render makes for the spec the flags
// build, with and without the token file, and writes no file and runs nothing.
func TestDaemonScratchRenderPrintsTheRenderAndWritesNothing(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"", "/run/secrets/om-token"} {
		r := newDaemonScratchRig(t)
		r.flags.omTokenFile = token
		want, err := templates.RenderDaemonCompose(r.spec())
		if err != nil {
			t.Fatal(err)
		}

		if err := runDaemonScratch(context.Background(), "render", r.flags, r.deps); err != nil {
			t.Fatalf("token %q: render: %v", token, err)
		}

		if r.out.String() != string(want) {
			t.Errorf("token %q: output differs from RenderDaemonCompose:\n%s", token, r.out.String())
		}
		if got := strings.Contains(r.out.String(), "--"+config.OMTokenFileFlag); got != (token != "") {
			t.Errorf("token %q: command carries the token flag = %v", token, got)
		}
		if r.composeExists() || len(r.runs) != 0 {
			t.Errorf("token %q: render wrote a file (%v) or ran %d commands", token, r.composeExists(), len(r.runs))
		}
	}
}

// up writes the compose file and then runs the start, through Deployer: the
// file is on disk with the render's content when the command runs, the argv is
// exactly the explicit-context up of the daemon service, and the shell's docker
// environment does not reach it.
func TestDaemonScratchUpWritesThenStarts(t *testing.T) {
	t.Parallel()
	r := newDaemonScratchRig(t)
	want, err := templates.RenderDaemonCompose(r.spec())
	if err != nil {
		t.Fatal(err)
	}

	if err := runDaemonScratch(context.Background(), "up", r.flags, r.deps); err != nil {
		t.Fatalf("up: %v", err)
	}

	if len(r.runs) != 1 {
		t.Fatalf("ran %d commands, want exactly the start", len(r.runs))
	}
	run := r.runs[0]
	wantArgv := []string{"docker", "--context", daemonimage.DockerContext, "compose", "-p", "gt-scratch", "-f", r.flags.composeFile, "up", "-d", "daemon"}
	if strings.Join(run.argv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("argv = %v, want %v", run.argv, wantArgv)
	}
	if run.compose != string(want) {
		t.Error("the compose file was not on disk with the render's content when the start ran")
	}
	for _, kv := range run.env {
		if strings.HasPrefix(kv, "DOCKER_CONTEXT=") || strings.HasPrefix(kv, "DOCKER_HOST=") {
			t.Errorf("the start ran with %s from the shell", strings.SplitN(kv, "=", 2)[0])
		}
	}
}

// A failed start is the command's error, and the compose file stays written.
func TestDaemonScratchUpReportsAFailedStart(t *testing.T) {
	t.Parallel()
	r := newDaemonScratchRig(t)
	r.deps.run = func(context.Context, string, []string, []string) error { return errors.New("docker: boom") }
	if err := runDaemonScratch(context.Background(), "up", r.flags, r.deps); err == nil || !strings.Contains(err.Error(), "docker: boom") {
		t.Fatalf("up = %v, want the start's error", err)
	}
	if !r.composeExists() {
		t.Error("the compose file was not left written after the failed start")
	}
}

// Both forge port flags make the render print the template's forge render for
// the same spec, and nothing else: no file, no command.
func TestDaemonScratchRenderPrintsTheForgeRenderWithBothPorts(t *testing.T) {
	t.Parallel()
	r := newDaemonScratchRig(t)
	r.setForge(3100, 2322)
	want, err := templates.RenderDaemonCompose(r.forgeSpec(3100, 2322))
	if err != nil {
		t.Fatal(err)
	}

	if err := runDaemonScratch(context.Background(), "render", r.flags, r.deps); err != nil {
		t.Fatalf("render: %v", err)
	}

	if r.out.String() != string(want) {
		t.Errorf("output differs from the template's forge render:\n%s", r.out.String())
	}
	for _, published := range []string{`"127.0.0.1:3100:3000"`, `"127.0.0.1:2322:22"`} {
		if !strings.Contains(r.out.String(), published) {
			t.Errorf("the render does not publish %s", published)
		}
	}
	if r.composeExists() || len(r.runs) != 0 {
		t.Errorf("render wrote a file (%v) or ran %d commands", r.composeExists(), len(r.runs))
	}
}

// Both forge port flags make up write that same render and start the forge's
// services beside the daemon: the forge services are not dependencies of
// daemon, so naming daemon alone would leave the forge stopped.
func TestDaemonScratchUpStartsTheForgeServicesBesideTheDaemon(t *testing.T) {
	t.Parallel()
	r := newDaemonScratchRig(t)
	r.setForge(3100, 2322)
	want, err := templates.RenderDaemonCompose(r.forgeSpec(3100, 2322))
	if err != nil {
		t.Fatal(err)
	}

	if err := runDaemonScratch(context.Background(), "up", r.flags, r.deps); err != nil {
		t.Fatalf("up: %v", err)
	}

	if len(r.runs) != 1 {
		t.Fatalf("ran %d commands, want exactly the start", len(r.runs))
	}
	run := r.runs[0]
	wantArgv := []string{"docker", "--context", daemonimage.DockerContext, "compose", "-p", "gt-scratch", "-f", r.flags.composeFile,
		"up", "-d", "daemon",
		templates.ScratchForgejoService, templates.ScratchForgejoDBService, templates.ScratchForgeDindService, templates.ScratchForgeRunnerService}
	if strings.Join(run.argv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("argv = %v, want %v", run.argv, wantArgv)
	}
	if run.compose != string(want) {
		t.Error("the compose file was not on disk with the forge render when the start ran")
	}
}

// Neither forge port flag leaves the forge off: the render carries no forge
// service and the start names the daemon alone.
func TestDaemonScratchWithoutForgePortsStartsTheDaemonAlone(t *testing.T) {
	t.Parallel()
	r := newDaemonScratchRig(t)

	if err := runDaemonScratch(context.Background(), "up", r.flags, r.deps); err != nil {
		t.Fatalf("up: %v", err)
	}

	if len(r.runs) != 1 {
		t.Fatalf("ran %d commands, want exactly the start", len(r.runs))
	}
	if got := r.runs[0].argv[len(r.runs[0].argv)-1]; got != "daemon" {
		t.Errorf("the start's last service is %q, want the daemon alone", got)
	}
	if strings.Contains(r.runs[0].compose, templates.ScratchForgejoService) {
		t.Error("the compose file carries the scratch forge without its ports being asked for")
	}
}

// A half port pair, a live Forgejo port, one port for both services and a value
// that is not a port are each refused before anything is written or run, for
// render and up alike. Each row is one check; a test fails if it is removed.
func TestDaemonScratchRefusesAHalfOrLiveForgePort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		forge forgePorts
		want  string
	}{
		{"http port alone", forgePorts{httpPort: 3100, httpSet: true}, "--forge-http-port 3100 is given without --forge-ssh-port"},
		{"ssh port alone", forgePorts{sshPort: 2322, sshSet: true}, "--forge-ssh-port 2322 is given without --forge-http-port"},
		{"the live Forgejo's http port", forgePorts{httpPort: 3000, sshPort: 2322, httpSet: true, sshSet: true}, "is a host port the live Forgejo publishes"},
		{"the live Forgejo's ssh port", forgePorts{httpPort: 3100, sshPort: 2222, httpSet: true, sshSet: true}, "is a host port the live Forgejo publishes"},
		{"one port for both services", forgePorts{httpPort: 3100, sshPort: 3100, httpSet: true, sshSet: true}, "need different host ports"},
		{"a port flag given as zero", forgePorts{httpPort: 0, sshPort: 2322, httpSet: true, sshSet: true}, "is not a port"},
	} {
		for _, action := range []string{"render", "up"} {
			t.Run(tc.name+" "+action, func(t *testing.T) {
				t.Parallel()
				r := newDaemonScratchRig(t)
				r.flags.forge = tc.forge

				err := runDaemonScratch(context.Background(), action, r.flags, r.deps)

				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%s error = %v, want it to contain %q", action, err, tc.want)
				}
				if r.out.Len() != 0 || len(r.runs) != 0 || r.composeExists() {
					t.Errorf("a refusal printed %d bytes, ran %d commands, wrote a file: %v", r.out.Len(), len(r.runs), r.composeExists())
				}
			})
		}
	}
}

// down runs the compose down -v of the scratch project and nothing else, in the
// compose file's directory, with the shell's docker environment dropped.
func TestDaemonScratchDownRemovesTheProjectOnly(t *testing.T) {
	t.Parallel()
	r := newDaemonScratchRig(t)
	if err := os.WriteFile(r.flags.composeFile, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runDaemonScratch(context.Background(), "down", r.flags, r.deps); err != nil {
		t.Fatalf("down: %v", err)
	}

	if len(r.runs) != 1 {
		t.Fatalf("ran %d commands, want exactly the down", len(r.runs))
	}
	run := r.runs[0]
	wantArgv := []string{"docker", "--context", daemonimage.DockerContext, "compose", "-p", "gt-scratch", "-f", r.flags.composeFile, "down", "-v"}
	if strings.Join(run.argv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("argv = %v, want %v", run.argv, wantArgv)
	}
	if run.dir != filepath.Dir(r.flags.composeFile) {
		t.Errorf("ran in %q, want the compose file's directory", run.dir)
	}
	for _, kv := range run.env {
		if strings.HasPrefix(kv, "DOCKER_CONTEXT=") || strings.HasPrefix(kv, "DOCKER_HOST=") {
			t.Errorf("down ran with %s from the shell", strings.SplitN(kv, "=", 2)[0])
		}
	}
}

// Every refusal comes before any file is written or command run. Each row is one
// check; a test fails if any one is removed.
func TestDaemonScratchRefusesBeforeAnyFileOrCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, r *daemonScratchRig)
		want string
	}{
		{"no --live-root", func(_ *testing.T, r *daemonScratchRig) { r.flags.liveRoot = "" }, "--live-root is required"},
		{"no --root", func(_ *testing.T, r *daemonScratchRig) { r.flags.root = "" }, "--root is required"},
		{"no --compose-file", func(_ *testing.T, r *daemonScratchRig) { r.flags.composeFile = "" }, "--compose-file is required"},
		{"no --image", func(_ *testing.T, r *daemonScratchRig) { r.flags.image = "" }, "--image is required"},
		{"no --project", func(_ *testing.T, r *daemonScratchRig) { r.flags.project = "" }, "both required"},
		{"no --container", func(_ *testing.T, r *daemonScratchRig) { r.flags.container = "" }, "both required"},
		{"the live container name", func(_ *testing.T, r *daemonScratchRig) { r.flags.container = config.DefaultContainerName }, "live daemon's container name"},
		{"the live project", func(_ *testing.T, r *daemonScratchRig) { r.flags.project = config.DefaultComposeProject }, "live compose project"},
		{"a project name of a bad shape", func(_ *testing.T, r *daemonScratchRig) { r.flags.project = "Bad Project" }, "daemon.compose_project"},
		{"a relative --live-root", func(_ *testing.T, r *daemonScratchRig) { r.flags.liveRoot = "live" }, "--live-root \"live\" is not absolute"},
		{"a relative --root", func(_ *testing.T, r *daemonScratchRig) { r.flags.root = "town" }, "--root \"town\" is not absolute"},
		{"a relative --compose-file", func(_ *testing.T, r *daemonScratchRig) { r.flags.composeFile = "compose.yml" }, "--compose-file \"compose.yml\" is not absolute"},
		{"a relative token file", func(_ *testing.T, r *daemonScratchRig) { r.flags.omTokenFile = "secrets/om-token" }, "--om-token-file \"secrets/om-token\" is not absolute"},
		{"the compose file inside --root", func(_ *testing.T, r *daemonScratchRig) {
			r.flags.composeFile = filepath.Join(r.flags.root, "compose.yml")
		}, "inside --root"},
		{"--live-root that is not a town", func(t *testing.T, r *daemonScratchRig) { r.flags.liveRoot = t.TempDir() }, "is not a town"},
		{"--live-root whose town.json is a link", func(t *testing.T, r *daemonScratchRig) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "mayor"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(r.flags.liveRoot, "mayor", "town.json"), filepath.Join(dir, "mayor", "town.json")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			r.flags.liveRoot = dir
		}, "is not a town"},
		{"--root equal to the live town", func(t *testing.T, r *daemonScratchRig) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(r.flags.liveRoot, config.ScratchTownMarker), nil, 0o600); err != nil {
				t.Fatalf("writing the marker: %v", err)
			}
			r.flags.root = r.flags.liveRoot
		}, "live town root"},
		{"--root under the live town", func(t *testing.T, r *daemonScratchRig) {
			under := filepath.Join(r.flags.liveRoot, "crew", "scratch")
			if err := os.MkdirAll(under, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(under, config.ScratchTownMarker), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			r.flags.root = under
		}, "or lies under it"},
		{"--root around the live town", func(t *testing.T, r *daemonScratchRig) {
			t.Helper()
			outer := filepath.Dir(r.flags.liveRoot)
			if err := os.WriteFile(filepath.Join(outer, config.ScratchTownMarker), nil, 0o600); err != nil {
				t.Fatalf("writing the marker: %v", err)
			}
			r.flags.root = outer
			r.flags.composeFile = filepath.Join(t.TempDir(), "compose.yml")
		}, "contains the live town root"},
		{"--root that does not exist", func(t *testing.T, r *daemonScratchRig) { r.flags.root = filepath.Join(t.TempDir(), "absent") }, "not an existing directory"},
		{"--root with no marker", func(t *testing.T, r *daemonScratchRig) {
			t.Helper()
			if err := os.Remove(filepath.Join(r.flags.root, config.ScratchTownMarker)); err != nil {
				t.Fatalf("removing the marker: %v", err)
			}
		}, "holds no .gt-scratch-town marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newDaemonScratchRig(t)
			tc.edit(t, r)

			err := runDaemonScratch(context.Background(), "up", r.flags, r.deps)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("up error = %v, want it to contain %q", err, tc.want)
			}
			if len(r.runs) != 0 {
				t.Errorf("a refusal ran %d commands", len(r.runs))
			}
			if r.composeExists() {
				t.Error("a refusal wrote the compose file")
			}
		})
	}
}

// down is refused without a compose file that exists, without validated scratch
// names and with a relative path, and runs nothing.
func TestDaemonScratchDownRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(r *daemonScratchRig)
		want string
	}{
		{"no --live-root", func(r *daemonScratchRig) { r.flags.liveRoot = "" }, "--live-root is required"},
		{"no compose file on disk", func(*daemonScratchRig) {}, "is not a file"},
		{"a relative compose file", func(r *daemonScratchRig) { r.flags.composeFile = "compose.yml" }, "is not absolute"},
		{"the live project", func(r *daemonScratchRig) { r.flags.project = config.DefaultComposeProject }, "live compose project"},
		{"the live container", func(r *daemonScratchRig) { r.flags.container = config.DefaultContainerName }, "live daemon's container name"},
		{"no names", func(r *daemonScratchRig) { r.flags.project, r.flags.container = "", "" }, "both required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newDaemonScratchRig(t)
			tc.edit(r)
			err := runDaemonScratch(context.Background(), "down", r.flags, r.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("down error = %v, want it to contain %q", err, tc.want)
			}
			if len(r.runs) != 0 {
				t.Errorf("a refusal ran %d commands", len(r.runs))
			}
		})
	}
}

func TestDaemonScratchUnknownAction(t *testing.T) {
	t.Parallel()
	r := newDaemonScratchRig(t)
	if err := runDaemonScratch(context.Background(), "restart", r.flags, r.deps); err == nil {
		t.Error("an unknown action ran")
	}
}

// The command registers render, up and down, parses the shared flags and the
// per-subcommand ones, and hands the action and flags over.
func TestDaemonScratchFlagsAreWired(t *testing.T) {
	t.Parallel()
	var gotAction string
	var got scratchFlags
	c := newDaemonScratchCmd(func(_ context.Context, action string, _ io.Writer, f scratchFlags) error {
		gotAction, got = action, f
		return nil
	})
	c.SetArgs([]string{"up", "--live-root", "/l", "--root", "/r", "--compose-file", "/c", "--image", "gt-daemon:abc1234",
		"--project", "gt-scratch", "--container", "gt-scratch-daemon-1", "--om-token-file", "/t",
		"--forge-http-port", "3100", "--forge-ssh-port", "2322"})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := scratchFlags{liveRoot: "/l", root: "/r", composeFile: "/c", image: "gt-daemon:abc1234", project: "gt-scratch", container: "gt-scratch-daemon-1", omTokenFile: "/t",
		forge: forgePorts{httpPort: 3100, sshPort: 2322, httpSet: true, sshSet: true}}
	if gotAction != "up" || got != want {
		t.Errorf("action %q flags %+v, want up and %+v", gotAction, got, want)
	}
	for _, name := range []string{"render", "down"} {
		sub, _, err := c.Find([]string{name})
		if err != nil || sub.Name() != name {
			t.Errorf("subcommand %s not registered (%v)", name, err)
		}
	}
	c2 := newDaemonScratchCmd(func(_ context.Context, action string, _ io.Writer, f scratchFlags) error {
		gotAction, got = action, f
		return nil
	})
	c2.SetArgs([]string{"down", "--compose-file", "/c", "--project", "p", "--container", "c"})
	c2.SilenceUsage, c2.SilenceErrors = true, true
	if err := c2.Execute(); err != nil || gotAction != "down" || got.composeFile != "/c" {
		t.Errorf("down: err %v action %q flags %+v", err, gotAction, got)
	}
}

func TestPathWithin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path, root string
		want       bool
	}{
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
		{"/x/y", "/a/b", false},
		{"/a/b/../c", "/a/b", false},
	} {
		if got := pathWithin(tc.path, tc.root); got != tc.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
		}
	}
}

// render refuses what up refuses, with nothing printed: the command's own flag
// checks (a missing --image) and the render's own (a scratch root that is the
// live town).
func TestDaemonScratchRenderRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(r *daemonScratchRig)
		want string
	}{
		{"no --image", func(r *daemonScratchRig) { r.flags.image = "" }, "--image is required"},
		{"a root that is the live town", func(r *daemonScratchRig) { r.flags.root = r.flags.liveRoot }, "live town root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newDaemonScratchRig(t)
			tc.edit(r)

			err := runDaemonScratch(context.Background(), "render", r.flags, r.deps)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("render error = %v, want it to contain %q", err, tc.want)
			}
			if r.out.Len() != 0 || len(r.runs) != 0 || r.composeExists() {
				t.Errorf("a refused render printed %d bytes, ran %d commands, wrote a file: %v", r.out.Len(), len(r.runs), r.composeExists())
			}
		})
	}
}

// scratchLiveSettings writes one live-town settings state for a name check: what
// the live town's daemon settings or settings file hold.
type scratchLiveSettings struct {
	name  string
	write func(t *testing.T, live string)
	want  string // "" means the scratch names pass
}

func writeLiveFile(t *testing.T, live, rel, body string) {
	t.Helper()
	p := filepath.Join(live, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", rel, err)
	}
}

func liveDirInstead(t *testing.T, live, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(live, rel), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
}

// render, up and down all refuse a scratch project or container that equals the
// name the live town's own settings give it, and fail closed on settings that
// cannot be read or parsed: down removes volumes, so "no override found" is never
// the answer to an error. A missing settings file means the compiled-in defaults
// only. The production readers run over the temp live town.
func TestDaemonScratchRefusesTheLiveTownsConfiguredNames(t *testing.T) {
	t.Parallel()
	cases := []scratchLiveSettings{
		{"no settings at all", func(*testing.T, string) {}, ""},
		{"settings that name other things", func(t *testing.T, live string) {
			writeLiveFile(t, live, "mayor/daemon.json", `{"type":"daemon-patrol-config","version":1,"container_name":"live-daemon-9","compose_project":"live-9"}`)
		}, ""},
		{"a settings file with no daemon section", func(t *testing.T, live string) {
			writeLiveFile(t, live, "settings/config.json", `{"unrelated": 1}`)
		}, ""},
		{"the project is a configured override", func(t *testing.T, live string) {
			writeLiveFile(t, live, "mayor/daemon.json", `{"type":"daemon-patrol-config","version":1,"container_name":"live-daemon-9","compose_project":"gt-scratch"}`)
		}, "is the live town's compose project"},
		{"the container is a configured override", func(t *testing.T, live string) {
			writeLiveFile(t, live, "mayor/daemon.json", `{"type":"daemon-patrol-config","version":1,"container_name":"gt-scratch-daemon-1","compose_project":"live-9"}`)
		}, "is the live town's container name"},
		{"the settings file does not parse", func(t *testing.T, live string) {
			writeLiveFile(t, live, "settings/config.json", `{not json`)
		}, "do not parse"},
		{"the settings file cannot be read", func(t *testing.T, live string) {
			liveDirInstead(t, live, "settings/config.json")
		}, "cannot be read"},
		{"the daemon settings do not parse", func(t *testing.T, live string) {
			writeLiveFile(t, live, "mayor/daemon.json", `{not json`)
		}, "daemon settings cannot be read"},
		{"the daemon settings cannot be read", func(t *testing.T, live string) {
			liveDirInstead(t, live, "mayor/daemon.json")
		}, "daemon settings cannot be read"},
	}
	for _, tc := range cases {
		for _, action := range []string{"render", "up", "down"} {
			t.Run(tc.name+" "+action, func(t *testing.T) {
				t.Parallel()
				r := newDaemonScratchRig(t)
				tc.write(t, r.flags.liveRoot)
				if action == "down" {
					if err := os.WriteFile(r.flags.composeFile, []byte("services: {}\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}

				err := runDaemonScratch(context.Background(), action, r.flags, r.deps)

				if tc.want == "" {
					if err != nil {
						t.Fatalf("%s: %v", action, err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%s error = %v, want it to contain %q", action, err, tc.want)
				}
				if len(r.runs) != 0 {
					t.Errorf("a refusal ran %d commands", len(r.runs))
				}
				if action == "up" && r.composeExists() {
					t.Error("a refused up wrote the compose file")
				}
			})
		}
	}
}

// A compose file inside the live town root is refused, on resolved paths: a
// directory link into the live town names it under another spelling.
func TestDaemonScratchRefusesAComposeFileInsideTheLiveRoot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, r *daemonScratchRig)
	}{
		{"a compose file under the live root", func(t *testing.T, r *daemonScratchRig) {
			t.Helper()
			r.flags.composeFile = filepath.Join(r.flags.liveRoot, "crew", "compose.yml")
		}},
		{"a compose file path that contains the live root", func(t *testing.T, r *daemonScratchRig) {
			t.Helper()
			r.flags.composeFile = filepath.Dir(r.flags.liveRoot)
		}},
		{"a compose file through a link into the live root", func(t *testing.T, r *daemonScratchRig) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(r.flags.liveRoot, "crew"), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(filepath.Join(r.flags.liveRoot, "crew"), link); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			r.flags.composeFile = filepath.Join(link, "compose.yml")
		}},
	} {
		for _, action := range []string{"render", "up", "down"} {
			t.Run(tc.name+" "+action, func(t *testing.T) {
				t.Parallel()
				r := newDaemonScratchRig(t)
				tc.edit(t, r)

				err := runDaemonScratch(context.Background(), action, r.flags, r.deps)

				if err == nil || !strings.Contains(err.Error(), "resolves inside the live town root") {
					t.Fatalf("%s error = %v, want the compose-file refusal", action, err)
				}
				if len(r.runs) != 0 || r.composeExists() {
					t.Errorf("a refusal ran %d commands or wrote the compose file", len(r.runs))
				}
			})
		}
	}
}

// ACCEPTED RISK pin: gt daemon scratch is exempt from the bd handshake because it
// never needs bd, and this keeps it so. Every command the three actions run goes
// through the runner, and none is bd or names one; a bd call added to any path
// turns this red, so the exemption cannot quietly grow into a path that needs the
// handshake.
func TestDaemonScratchNeverRunsBD(t *testing.T) {
	t.Parallel()
	r := newDaemonScratchRig(t)
	for _, action := range []string{"render", "up", "down"} {
		if err := runDaemonScratch(context.Background(), action, r.flags, r.deps); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if len(r.runs) != 2 {
		t.Fatalf("ran %d commands for render, up and down, want 2 (the start and the down)", len(r.runs))
	}
	for _, run := range r.runs {
		joined := strings.Join(run.argv, " ")
		if filepath.Base(run.argv[0]) == "bd" || strings.Contains(joined, "bd ") || strings.HasPrefix(joined, "bd") {
			t.Errorf("a scratch command ran bd: %v", run.argv)
		}
	}
}

// resolveThroughExisting resolves the nearest existing ancestor and appends the
// rest, and returns an error that is not "does not exist" as it is.
func TestResolveThroughExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolveThroughExisting(filepath.Join(link, "a", "b.yml"), filepath.EvalSymlinks)
	if err != nil || got != filepath.Join(resolvedReal, "a", "b.yml") {
		t.Errorf("resolveThroughExisting = %q, %v; want the link resolved and the absent tail appended", got, err)
	}
	boom := errors.New("permission denied")
	if _, err := resolveThroughExisting("/x/y", func(string) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Errorf("a non-ENOENT error = %v, want it returned", err)
	}
	if _, err := resolveThroughExisting("/x/y", func(string) (string, error) { return "", os.ErrNotExist }); err == nil {
		t.Error("a path with no existing ancestor, not even the root, resolved")
	}
}

// A live root or a compose file whose path cannot be resolved is refused, not
// taken as clear: "cannot tell" is never "outside the live town".
func TestDaemonScratchRefusesPathsItCannotResolve(t *testing.T) {
	t.Parallel()
	boom := errors.New("permission denied")
	for _, tc := range []struct {
		name string
		eval func(r *daemonScratchRig) func(string) (string, error)
		want string
	}{
		{"the live root", func(r *daemonScratchRig) func(string) (string, error) {
			return func(p string) (string, error) {
				if p == r.flags.liveRoot {
					return "", boom
				}
				return p, nil
			}
		}, "--live-root"},
		{"the compose file", func(r *daemonScratchRig) func(string) (string, error) {
			return func(p string) (string, error) {
				if p == r.flags.liveRoot {
					return p, nil
				}
				return "", boom
			}
		}, "--compose-file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newDaemonScratchRig(t)
			r.deps.evalSymlinks = tc.eval(r)

			err := refuseComposeFileInLiveRoot(r.flags, r.deps)

			if err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), tc.want+" ") || !strings.Contains(err.Error(), "cannot be resolved") {
				t.Fatalf("refuseComposeFileInLiveRoot = %v, want a %s refusal wrapping the resolve error", err, tc.want)
			}
		})
	}
}

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

func (r *daemonScratchRig) composeExists() bool {
	_, err := os.Stat(r.flags.composeFile)
	return err == nil
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
		{"--root equal to the live town", func(_ *testing.T, r *daemonScratchRig) {
			if err := os.WriteFile(filepath.Join(r.flags.liveRoot, config.ScratchTownMarker), nil, 0o600); err != nil {
				panic(err)
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
			outer := filepath.Dir(r.flags.liveRoot)
			if err := os.WriteFile(filepath.Join(outer, config.ScratchTownMarker), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			r.flags.root = outer
			r.flags.composeFile = filepath.Join(t.TempDir(), "compose.yml")
		}, "contains the live town root"},
		{"--root that does not exist", func(t *testing.T, r *daemonScratchRig) { r.flags.root = filepath.Join(t.TempDir(), "absent") }, "not an existing directory"},
		{"--root with no marker", func(_ *testing.T, r *daemonScratchRig) {
			if err := os.Remove(filepath.Join(r.flags.root, config.ScratchTownMarker)); err != nil {
				panic(err)
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
		"--project", "gt-scratch", "--container", "gt-scratch-daemon-1", "--om-token-file", "/t"})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := scratchFlags{liveRoot: "/l", root: "/r", composeFile: "/c", image: "gt-daemon:abc1234", project: "gt-scratch", container: "gt-scratch-daemon-1", omTokenFile: "/t"}
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

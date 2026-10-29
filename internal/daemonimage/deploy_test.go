package daemonimage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// fakeSupervisor is a container supervisor whose start argv is fixed, or whose
// build refuses the way the landed one does for a town that does not set both
// container settings (gt-qpeei.10). It counts the builds, so a test can prove a
// refusal came before the argv was ever asked for.
type fakeSupervisor struct {
	argv   []string
	refuse error
	built  int
}

func (s *fakeSupervisor) StartArgv() ([]string, error) {
	s.built++
	if s.refuse != nil {
		return nil, s.refuse
	}
	return append([]string(nil), s.argv...), nil
}

// deployRunner records each command it is given, and the compose file's content
// at the moment it ran (nil when the file was absent), so a test can prove the
// write landed before the start command ran. fail, when set, is what a command
// returns.
type deployRunner struct {
	composePath string
	calls       []recorded
	composeAt   [][]byte
	fail        error
}

func (r *deployRunner) run(_ context.Context, dir string, env, argv []string) error {
	r.calls = append(r.calls, recorded{
		dir:  dir,
		env:  append([]string(nil), env...),
		argv: append([]string(nil), argv...),
	})
	content, err := os.ReadFile(r.composePath)
	if err != nil {
		content = nil
	}
	r.composeAt = append(r.composeAt, content)
	return r.fail
}

// deploySpec is the spec a town with its own project renders: its own project
// and container name, the deployed image tag, an absolute town root and a Dolt
// host reachable from inside the container. Every field passes RenderDaemonCompose.
func deploySpec() templates.ComposeSpec {
	return templates.ComposeSpec{
		Project:       "scratch",
		ContainerName: "scratch-daemon-1",
		Image:         ImageTag(testRev),
		TownRoot:      "/tmp/scratch-town",
		StopGrace:     templates.MinimumStopGrace,
		DoltHost:      "host.docker.internal",
	}
}

// containerStartArgv is what the landed start builder returns for a container
// town whose compose project is scratch and whose compose file is composePath:
// `up -d daemon` in the explicit Docker Desktop context (gt-qpeei.10, A2's own
// acceptance). A test pins Deploy to exactly this argv, so the two never drift.
func containerStartArgv(composePath string) []string {
	return []string{"docker", "--context", "desktop-linux", "compose", "-p", "scratch", "-f", composePath, "up", "-d", "daemon"}
}

// composePathIn makes the directory a town's compose file lives in and returns
// its path: <dir>/daemon/container/compose.yml.
func composePathIn(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "daemon", "container", "compose.yml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("creating the compose directory: %v", err)
	}
	return p
}

// oldCompose is the file a town's compose path holds before a deploy: a render
// of an earlier image, distinguishable from the new one.
var oldCompose = []byte("services:\n  daemon:\n    image: gt-daemon:older\n")

// writeOldCompose puts oldCompose at path and returns it.
func writeOldCompose(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, oldCompose, 0o644); err != nil {
		t.Fatalf("writing the old compose file: %v", err)
	}
}

// readCompose is what path holds now.
func readCompose(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return content
}

// onlyFileIn fails when dir holds anything but name, so a test notices a
// temporary render left behind.
func onlyFileIn(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %v, want only %s", dir, names, name)
	}
}

// TestDeployWritesTheComposeFileBeforeItStartsTheContainer: the start command
// is the town's own argv, it runs after the new render is on disk, and it runs
// in the explicit context even when the host environment names another engine.
func TestDeployWritesTheComposeFileBeforeItStartsTheContainer(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	writeOldCompose(t, path)
	run := &deployRunner{composePath: path}
	sup := &fakeSupervisor{argv: containerStartArgv(path)}
	d := Deployer{
		Run:     run.run,
		Environ: []string{"PATH=/usr/bin:/bin", "HOME=/home/builder", "DOCKER_HOST=tcp://elsewhere:2375", "DOCKER_CONTEXT=elsewhere"},
	}
	if err := d.Deploy(context.Background(), sup, path, deploySpec()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	want, err := templates.RenderDaemonCompose(deploySpec())
	if err != nil {
		t.Fatalf("rendering the expected compose file: %v", err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("recorded %d commands, want the one start command", len(run.calls))
	}
	if got := run.calls[0].argv; !reflect.DeepEqual(got, containerStartArgv(path)) {
		t.Errorf("start argv = %q, want %q", got, containerStartArgv(path))
	}
	if got := run.composeAt[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("the compose file the start command saw = %q, want the new render; Deploy must write the file before it starts the container", got)
	}
	if got := readCompose(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("the compose file on disk = %q, want the new render", got)
	}
	if run.calls[0].dir != filepath.Dir(path) {
		t.Errorf("the start command ran in %s, want the compose file's directory %s", run.calls[0].dir, filepath.Dir(path))
	}
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/builder"} {
		if !contains(run.calls[0].env, want) {
			t.Errorf("start command env = %q, want %s among them", run.calls[0].env, want)
		}
	}
	for _, banned := range []string{"DOCKER_HOST=tcp://elsewhere:2375", "DOCKER_CONTEXT=elsewhere"} {
		if contains(run.calls[0].env, banned) {
			t.Errorf("start command env = %q, want %s dropped: the explicit --context decides the engine", run.calls[0].env, banned)
		}
	}
	onlyFileIn(t, filepath.Dir(path), filepath.Base(path))
}

// TestDeployRenderErrorLeavesTheOldComposeFileAndRunsNothing: a spec the
// renderer refuses changes nothing on disk and starts no container.
func TestDeployRenderErrorLeavesTheOldComposeFileAndRunsNothing(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	writeOldCompose(t, path)
	run := &deployRunner{composePath: path}
	sup := &fakeSupervisor{argv: containerStartArgv(path)}
	spec := deploySpec()
	spec.Image = ""
	err := Deployer{Run: run.run}.Deploy(context.Background(), sup, path, spec)
	if err == nil {
		t.Fatal("Deploy accepted a spec with no image")
	}
	if got := readCompose(t, path); !reflect.DeepEqual(got, oldCompose) {
		t.Errorf("the compose file = %q, want the old one unchanged", got)
	}
	if len(run.calls) != 0 {
		t.Errorf("recorded %d commands, want none after a render error", len(run.calls))
	}
	onlyFileIn(t, filepath.Dir(path), filepath.Base(path))
}

// TestDeploySupervisorRefusalLeavesTheOldComposeFileAndRunsNothing: a town that
// does not set both container settings is refused by the argv builder, and the
// refusal is returned before the compose file is replaced.
func TestDeploySupervisorRefusalLeavesTheOldComposeFileAndRunsNothing(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	writeOldCompose(t, path)
	run := &deployRunner{composePath: path}
	refusal := errors.New("daemon.supervisor is container, but the town does not set both daemon.container_name and daemon.compose_project")
	sup := &fakeSupervisor{refuse: refusal}
	err := Deployer{Run: run.run}.Deploy(context.Background(), sup, path, deploySpec())
	if !errors.Is(err, refusal) {
		t.Errorf("Deploy error = %v, want the start builder's refusal", err)
	}
	if got := readCompose(t, path); !reflect.DeepEqual(got, oldCompose) {
		t.Errorf("the compose file = %q, want the old one unchanged after a refusal", got)
	}
	if len(run.calls) != 0 {
		t.Errorf("recorded %d commands, want none after a refusal", len(run.calls))
	}
}

// TestDeployStartFailureIsReturned: the new render stays on disk (the file is
// what the next start reads) and the failed command's error reaches the caller.
func TestDeployStartFailureIsReturned(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	writeOldCompose(t, path)
	failure := errors.New("docker: no such context")
	run := &deployRunner{composePath: path, fail: failure}
	sup := &fakeSupervisor{argv: containerStartArgv(path)}
	err := Deployer{Run: run.run}.Deploy(context.Background(), sup, path, deploySpec())
	if !errors.Is(err, failure) {
		t.Errorf("Deploy error = %v, want the start command's failure", err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("recorded %d commands, want the one that failed", len(run.calls))
	}
	want, err := templates.RenderDaemonCompose(deploySpec())
	if err != nil {
		t.Fatalf("rendering the expected compose file: %v", err)
	}
	if got := readCompose(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("the compose file = %q, want the new render the next start reads", got)
	}
}

// TestDeployWriteErrorLeavesTheOldComposeFileAndRunsNothing: a compose path the
// write cannot replace (a directory), or a temporary path that is one, fails the
// deploy without starting anything and leaves no partial render behind.
func TestDeployWriteErrorLeavesTheOldComposeFileAndRunsNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// block makes the deploy's write fail: the path it writes the render to,
		// or the path it renames over.
		block func(t *testing.T, path string)
	}{
		{name: "the temporary path is a directory", block: func(t *testing.T, path string) {
			if err := os.Mkdir(path+".tmp", 0o755); err != nil {
				t.Fatalf("making the temporary path a directory: %v", err)
			}
		}},
		{name: "the compose path is a directory", block: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatalf("making the compose path a directory: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "compose.yml")
			tt.block(t, path)
			run := &deployRunner{composePath: path}
			sup := &fakeSupervisor{argv: containerStartArgv(path)}
			if err := (Deployer{Run: run.run}).Deploy(context.Background(), sup, path, deploySpec()); err == nil {
				t.Fatal("Deploy reported success writing where it cannot")
			}
			if len(run.calls) != 0 {
				t.Errorf("recorded %d commands, want none after a write error", len(run.calls))
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("reading %s: %v", dir, err)
			}
			if len(entries) != 1 {
				t.Errorf("%s holds %d entries after a failed write, want only the path the test blocked", dir, len(entries))
			}
		})
	}
}

// TestDeployRefusesAnIncompleteCall: a deploy with no runner or no supervisor
// asks for no argv and runs nothing.
func TestDeployRefusesAnIncompleteCall(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	writeOldCompose(t, path)
	tests := []struct {
		name string
		d    Deployer
		sup  Supervisor
	}{
		{name: "no runner", d: Deployer{}, sup: &fakeSupervisor{argv: containerStartArgv(path)}},
		{name: "no supervisor", d: Deployer{Run: func(context.Context, string, []string, []string) error { return nil }}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.d.Deploy(context.Background(), tt.sup, path, deploySpec()); err == nil {
				t.Fatal("Deploy accepted an incomplete call")
			}
			if sup, ok := tt.sup.(*fakeSupervisor); ok && sup.built != 0 {
				t.Errorf("the start argv was built %d times, want a refusal before it is asked for", sup.built)
			}
			if got := readCompose(t, path); !reflect.DeepEqual(got, oldCompose) {
				t.Errorf("the compose file = %q, want the old one unchanged", got)
			}
		})
	}
}

// TestDeployRefusesAComposePathItCannotName: the compose file's path must be
// non-empty and absolute, because the supervisor's argv names it and docker
// resolves a relative -f against its own directory.
func TestDeployRefusesAComposePathItCannotName(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	writeOldCompose(t, path)
	for _, bad := range []string{"", "  ", "daemon/container/compose.yml"} {
		sup := &fakeSupervisor{argv: containerStartArgv(path)}
		err := Deployer{Run: func(context.Context, string, []string, []string) error { return nil }}.Deploy(context.Background(), sup, bad, deploySpec())
		if err == nil {
			t.Errorf("Deploy accepted compose path %q", bad)
			continue
		}
		if !strings.Contains(err.Error(), "compose") {
			t.Errorf("Deploy(%q) error = %v, want it to name the compose file", bad, err)
		}
		if sup.built != 0 {
			t.Errorf("Deploy(%q) built the start argv, want a refusal before it is asked for", bad)
		}
	}
}

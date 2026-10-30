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

// deployProject and deployContainer are the project and container a town's
// settings name. The spec renders for them and the start argv runs under them.
const (
	deployProject   = "scratch"
	deployContainer = "scratch-daemon-1"
)

// fakeSupervisor is a container supervisor whose start argv is fixed, or whose
// build refuses the way the landed one does for a town that does not set both
// container settings (gt-qpeei.10). It names the container its settings give.
// It counts the builds, so a test can prove a refusal came before the argv was
// ever asked for.
type fakeSupervisor struct {
	argv      []string
	refuse    error
	container string
	built     int
}

func (s *fakeSupervisor) StartArgv() ([]string, error) {
	s.built++
	if s.refuse != nil {
		return nil, s.refuse
	}
	return append([]string(nil), s.argv...), nil
}

func (s *fakeSupervisor) ContainerName() string { return s.container }

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
		Project:       deployProject,
		ContainerName: deployContainer,
		Image:         ImageTag(testRev),
		TownRoot:      "/tmp/scratch-town",
		StopGrace:     templates.MinimumStopGrace,
		DoltHost:      "host.docker.internal",
	}
}

// containerStartArgv is a start argv in the shape the container supervisor
// builds: `up -d daemon` in the explicit Docker Desktop context, against
// composePath. It is a fixture for Deploy's own behaviour; that the real builder
// returns this argv is pinned by the test beside the supervisor in internal/cmd.
func containerStartArgv(composePath string) []string {
	return composeArgvFor(deployProject, composePath)
}

// composeArgvFor is containerStartArgv for a given project and compose file.
func composeArgvFor(project, composePath string) []string {
	return []string{"docker", "--context", "desktop-linux", "compose", "-p", project, "-f", composePath, "up", "-d", "daemon"}
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
	sup := &fakeSupervisor{container: deployContainer, argv: containerStartArgv(path)}
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
	sup := &fakeSupervisor{container: deployContainer, argv: containerStartArgv(path)}
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
	sup := &fakeSupervisor{container: deployContainer, refuse: refusal}
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
	sup := &fakeSupervisor{container: deployContainer, argv: containerStartArgv(path)}
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

// TestDeployWriteErrorLeavesNothingBehindAndRunsNothing: a render that cannot be
// written, because its directory is missing or its compose path is a directory,
// fails the deploy without starting anything and leaves no temporary file.
func TestDeployWriteErrorLeavesNothingBehindAndRunsNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup makes the write fail and returns the compose path the deploy
		// names, inside dir.
		setup func(t *testing.T, dir string) string
		// want is what dir holds after the failed deploy.
		want []string
	}{
		{name: "the compose directory does not exist", setup: func(t *testing.T, dir string) string {
			return filepath.Join(dir, "absent", "compose.yml")
		}},
		{name: "the compose path is a directory", setup: func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "compose.yml")
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatalf("making the compose path a directory: %v", err)
			}
			return p
		}, want: []string{"compose.yml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := tt.setup(t, dir)
			run := &deployRunner{composePath: path}
			sup := &fakeSupervisor{container: deployContainer, argv: containerStartArgv(path)}
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
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Errorf("%s holds %v after a failed write, want %v", dir, names, tt.want)
			}
		})
	}
}

// faultyTemp is a real temporary file that fails the step named: its write, its
// mode change or its close. The file is still created and closed for real, so
// a failure leaves the same files a real one would.
type faultyTemp struct {
	*os.File
	write, chmod, close error
}

func (f faultyTemp) Write(p []byte) (int, error) {
	if f.write != nil {
		return 0, f.write
	}
	return f.File.Write(p)
}

func (f faultyTemp) Chmod(mode os.FileMode) error {
	if f.chmod != nil {
		return f.chmod
	}
	return f.File.Chmod(mode)
}

func (f faultyTemp) Close() error {
	err := f.File.Close()
	if f.close != nil {
		return f.close
	}
	return err
}

// TestWriteComposeFileKeepsTheOldRenderWhenAStepFails: a write, a mode change or
// a close that fails leaves the old compose file as it was and no temporary file
// behind, and the failure is what the caller gets.
func TestWriteComposeFileKeepsTheOldRenderWhenAStepFails(t *testing.T) {
	t.Parallel()
	failure := errors.New("injected failure")
	tests := []struct {
		name                string
		write, chmod, close error
	}{
		{name: "the write fails", write: failure},
		{name: "the mode change fails", chmod: failure},
		{name: "the close fails", close: failure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := composePathIn(t)
			writeOldCompose(t, path)
			create := func(dir, pattern string) (composeTemp, error) {
				f, err := os.CreateTemp(dir, pattern)
				if err != nil {
					return nil, err
				}
				return faultyTemp{File: f, write: tt.write, chmod: tt.chmod, close: tt.close}, nil
			}
			if err := writeComposeFileWith(path, []byte("services: {}\n"), create); !errors.Is(err, failure) {
				t.Errorf("writeComposeFileWith error = %v, want the injected failure", err)
			}
			if got := readCompose(t, path); !reflect.DeepEqual(got, oldCompose) {
				t.Errorf("the compose file = %q, want the old one unchanged", got)
			}
			onlyFileIn(t, filepath.Dir(path), filepath.Base(path))
		})
	}
}

// TestDeployRefusesAStartCommandThatNamesOtherComposeFiles: the start command
// must name the one compose file Deploy writes, and only that one. A command
// that names another, several, or none would start a container from a file this
// deploy did not write, so it is refused before anything is written or run.
func TestDeployRefusesAStartCommandThatNamesOtherComposeFiles(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	writeOldCompose(t, path)
	other := filepath.Join(filepath.Dir(path), "other.yml")
	tests := []struct {
		name string
		argv []string
	}{
		{name: "another file", argv: containerStartArgv(other)},
		{name: "the file and another", argv: []string{"docker", "compose", "-p", deployProject, "-f", path, "-f", other, "up", "-d", "daemon"}},
		{name: "no file", argv: []string{"docker", "compose", "-p", deployProject, "up", "-d", "daemon"}},
		{name: "a file flag with no value", argv: []string{"docker", "compose", "-p", deployProject, "up", "-d", "daemon", "-f"}},
		{name: "a file flag after the subcommand", argv: []string{"docker", "compose", "-p", deployProject, "up", "-d", "daemon", "-f", path}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &deployRunner{composePath: path}
			sup := &fakeSupervisor{container: deployContainer, argv: tt.argv}
			if err := (Deployer{Run: run.run}).Deploy(context.Background(), sup, path, deploySpec()); err == nil {
				t.Fatalf("Deploy accepted the start command %q", tt.argv)
			}
			if len(run.calls) != 0 {
				t.Errorf("recorded %d commands, want none for a command that names another file", len(run.calls))
			}
			if got := readCompose(t, path); !reflect.DeepEqual(got, oldCompose) {
				t.Errorf("the compose file = %q, want the old one unchanged", got)
			}
		})
	}
}

// TestDeployAcceptsTheComposeFileAndProjectInEitherFlagForm: -f and --file name
// the file, and -p and --project-name name the project, whether each value
// follows its flag or is joined to it with =.
func TestDeployAcceptsTheComposeFileAndProjectInEitherFlagForm(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	for _, argv := range [][]string{
		{"docker", "compose", "--project-name", deployProject, "--file", path, "up", "-d", "daemon"},
		{"docker", "compose", "--project-name=" + deployProject, "--file=" + path, "up", "-d", "daemon"},
		{"docker", "compose", "-p=" + deployProject, "-f=" + path, "up", "-d", "daemon"},
	} {
		run := &deployRunner{composePath: path}
		sup := &fakeSupervisor{container: deployContainer, argv: argv}
		if err := (Deployer{Run: run.run}).Deploy(context.Background(), sup, path, deploySpec()); err != nil {
			t.Errorf("Deploy(%q): %v", argv, err)
		}
		if len(run.calls) != 1 {
			t.Errorf("Deploy(%q) recorded %d commands, want the start command", argv, len(run.calls))
		}
	}
}

// TestDeployRefusesAStartCommandForAnotherProject: the start command must run
// the one compose project the render is for. A command that runs another, several,
// or none would recreate a container this render does not describe, so it is
// refused before anything is written or run.
func TestDeployRefusesAStartCommandForAnotherProject(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	writeOldCompose(t, path)
	tests := []struct {
		name string
		argv []string
	}{
		{name: "another project", argv: composeArgvFor("live", path)},
		{name: "two projects", argv: []string{"docker", "compose", "-p", deployProject, "-p", "live", "-f", path, "up", "-d", "daemon"}},
		{name: "no project", argv: []string{"docker", "compose", "-f", path, "up", "-d", "daemon"}},
		{name: "a project flag with no value", argv: []string{"docker", "compose", "-p", "-f", path, "up", "-d", "daemon"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &deployRunner{composePath: path}
			sup := &fakeSupervisor{container: deployContainer, argv: tt.argv}
			if err := (Deployer{Run: run.run}).Deploy(context.Background(), sup, path, deploySpec()); err == nil {
				t.Fatalf("Deploy accepted the start command %q", tt.argv)
			}
			if len(run.calls) != 0 {
				t.Errorf("recorded %d commands, want none for a command that runs another project", len(run.calls))
			}
			if got := readCompose(t, path); !reflect.DeepEqual(got, oldCompose) {
				t.Errorf("the compose file = %q, want the old one unchanged", got)
			}
		})
	}
}

// TestDeployRefusesARenderForAnotherContainer: the render names the container the
// town's settings name, which is the one the start command recreates. A render
// for any other container would leave that container running the old image while
// the file describes a new one, so it is refused before anything is written.
func TestDeployRefusesARenderForAnotherContainer(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	writeOldCompose(t, path)
	run := &deployRunner{composePath: path}
	sup := &fakeSupervisor{container: deployContainer, argv: containerStartArgv(path)}
	spec := deploySpec()
	spec.ContainerName = "live-daemon-1"
	if err := (Deployer{Run: run.run}).Deploy(context.Background(), sup, path, spec); err == nil {
		t.Fatal("Deploy accepted a render for a container the town's settings do not name")
	}
	if len(run.calls) != 0 {
		t.Errorf("recorded %d commands, want none for a render for another container", len(run.calls))
	}
	if got := readCompose(t, path); !reflect.DeepEqual(got, oldCompose) {
		t.Errorf("the compose file = %q, want the old one unchanged", got)
	}
	onlyFileIn(t, filepath.Dir(path), filepath.Base(path))
}

// TestDeployReadsOnlyTheFlagsBeforeTheSubcommand: the compose flags a command
// carries belong before its subcommand. A file flag after `up` is the
// subcommand's own argument, so it does not count as a second compose file.
func TestDeployReadsOnlyTheFlagsBeforeTheSubcommand(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	run := &deployRunner{composePath: path}
	sup := &fakeSupervisor{container: deployContainer, argv: []string{
		"docker", "compose", "-p", deployProject, "-f", path, "up", "-d", "daemon", "-f", filepath.Join(filepath.Dir(path), "other.yml"),
	}}
	if err := (Deployer{Run: run.run}).Deploy(context.Background(), sup, path, deploySpec()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(run.calls) != 1 {
		t.Errorf("recorded %d commands, want the start command", len(run.calls))
	}
}

// TestDeployLeavesOtherFilesBesideTheComposeFileAlone: the render's temporary
// file is a new name, so a file a deploy does not own, even one at the old
// fixed temporary name, is never overwritten by it.
func TestDeployLeavesOtherFilesBesideTheComposeFileAlone(t *testing.T) {
	t.Parallel()
	path := composePathIn(t)
	writeOldCompose(t, path)
	keep := []byte("someone else's file\n")
	if err := os.WriteFile(path+".tmp", keep, 0o644); err != nil {
		t.Fatalf("writing a neighbouring file: %v", err)
	}
	run := &deployRunner{composePath: path}
	sup := &fakeSupervisor{container: deployContainer, argv: containerStartArgv(path)}
	if err := (Deployer{Run: run.run}).Deploy(context.Background(), sup, path, deploySpec()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if got := readCompose(t, path+".tmp"); !reflect.DeepEqual(got, keep) {
		t.Errorf("the neighbouring file = %q, want it untouched", got)
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
		{name: "no runner", d: Deployer{}, sup: &fakeSupervisor{container: deployContainer, argv: containerStartArgv(path)}},
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

// TestImageTagRev: the tag reader beside ImageTag names the rev of a tag the
// builder could have made, and refuses every other tag.
func TestImageTagRev(t *testing.T) {
	t.Parallel()
	const rev = "0123456"
	if got, err := ImageTagRev(ImageTag(rev)); err != nil || got != rev {
		t.Errorf("ImageTagRev(ImageTag(%q)) = %q, %v; want the rev and no error", rev, got, err)
	}
	for _, tag := range []string{
		"",
		"gt-daemon:",
		"gt-daemon:abc12",
		"gt-daemon:012345g",
		"gt-daemon:/0123456",
		"daemon:0123456",
		"gt-daemon:0123456789012345678901234567890123456789012",
	} {
		if got, err := ImageTagRev(tag); err == nil {
			t.Errorf("ImageTagRev(%q) = %q, want a refusal", tag, got)
		}
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
		sup := &fakeSupervisor{container: deployContainer, argv: containerStartArgv(path)}
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

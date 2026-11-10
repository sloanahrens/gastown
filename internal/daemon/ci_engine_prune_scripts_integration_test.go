//go:build integration

package daemon

// What this tier proves, and what it does not. It proves that the list program,
// the one shell text the prune runs as root inside the runner's dind, runs under a
// real sh and prints JSON the daemon parses; that each removal and presence argv
// the daemon builds reaches a docker binary as the verb and the ids with nothing
// shell-quoted between them; and that a non-zero exit comes back as a non-zero
// exit. It does not prove docker's output shapes or flags: the fake docker prints
// the text each case writes for it, so a case shows that the parser reads that
// text, not that docker prints it. The shapes are held by the captures in
// TestCIEngineRemovedReadsDockersOutput and TestCIEnginePresentReadsInspectOutput,
// taken from the runner's dind (gt-i02983).

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDockerEnv is the fake docker a case runs against: a directory holding the
// docker script, the call log, the canned output per verb, and the verbs to fail.
type fakeDockerEnv struct {
	dir string
}

// newFakeDocker writes the fake docker and its directories under t.TempDir().
func newFakeDocker(t *testing.T) fakeDockerEnv {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"bin", "out", "fail"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("making %s: %v", sub, err)
		}
	}
	// The verb key is the command, with the object word for the verbs that take one.
	const script = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DOCKER_DIR/calls"
case "$1" in container|image|volume|builder) key="$1-$2" ;; *) key="$1" ;; esac
if [ -f "$FAKE_DOCKER_DIR/fail/$key" ]; then echo "fake: $key failed" >&2; exit 1; fi
if [ -f "$FAKE_DOCKER_DIR/out/$key" ]; then cat "$FAKE_DOCKER_DIR/out/$key"; fi
`
	if err := os.WriteFile(filepath.Join(dir, "bin", "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the fake docker: %v", err)
	}
	return fakeDockerEnv{dir: dir}
}

// canned sets the output a verb prints.
func (f fakeDockerEnv) canned(t *testing.T, key, out string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "out", key), []byte(out), 0o644); err != nil {
		t.Fatalf("writing the canned output for %s: %v", key, err)
	}
}

// failing makes a verb exit non-zero.
func (f fakeDockerEnv) failing(t *testing.T, key string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "fail", key), nil, 0o644); err != nil {
		t.Fatalf("marking %s failing: %v", key, err)
	}
}

// calls returns the docker calls the programs made, one line each, in order.
func (f fakeDockerEnv) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the call log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

// runProgram runs a shell program with the fake docker first on PATH and the
// given argv, returning its stdout and whether it exited zero.
func (f fakeDockerEnv) runProgram(t *testing.T, program string, args ...string) ([]byte, bool) {
	t.Helper()
	argv := append([]string{"-c", program, "program"}, args...)
	cmd := exec.Command("sh", argv...)
	cmd.Env = []string{
		"PATH=" + filepath.Join(f.dir, "bin") + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"FAKE_DOCKER_DIR=" + f.dir,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Logf("program exited: %v: %s", err, stderr.String())
	}
	return stdout.Bytes(), err == nil
}

// runArgv runs one command with the fake docker first on PATH, returning its
// stdout and whether it exited zero.
func (f fakeDockerEnv) runArgv(t *testing.T, argv ...string) ([]byte, bool) {
	t.Helper()
	// The program is the fake under f.dir by path: exec.Command resolves a bare
	// name through this process's PATH, not the child's, and must never reach a
	// real docker.
	cmd := exec.Command(filepath.Join(f.dir, "bin", argv[0]), argv[1:]...)
	cmd.Env = []string{
		"PATH=" + filepath.Join(f.dir, "bin") + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"FAKE_DOCKER_DIR=" + f.dir,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Logf("%s exited: %v: %s", argv[0], err, stderr.String())
	}
	return stdout.Bytes(), err == nil
}

// TestIntegrationCIEngineRemovalsReachDockerAsGiven: each removal's argv reaches
// docker as the verb and the ids, nothing else, and docker's output read back
// names every id removed.
func TestIntegrationCIEngineRemovalsReachDockerAsGiven(t *testing.T) {
	f := newFakeDocker(t)
	plan := ciEnginePlan{
		containers: []ciEngineContainer{{ID: "c1"}, {ID: "c2"}},
		images:     []ciEngineImage{{ID: "sha256:i1"}},
		volumes:    []ciEngineVolume{{Name: "v1"}, {Name: "v2"}},
	}
	f.canned(t, "rm", "c1\nc2\n")
	f.canned(t, "rmi", "Untagged: some:tag\nDeleted: sha256:i1\n")
	f.canned(t, "volume-rm", "v1\nv2\n")
	for _, r := range ciEngineRemovals(plan) {
		out, ok := f.runArgv(t, r.argv...)
		if !ok {
			t.Fatalf("the %s removal failed on a clean engine", r.kind)
		}
		if missing := ciEngineMissing(r, out); len(missing) != 0 {
			t.Errorf("the %s removal left %q unreported by docker", r.kind, missing)
		}
	}
	if _, ok := f.runArgv(t, ciEngineBuildCachePruneArgv("2026-11-09T00:00:00Z")...); !ok {
		t.Fatalf("the build cache prune failed on a clean engine")
	}
	want := []string{
		"rm c1 c2",
		"rmi -f sha256:i1",
		"volume rm v1 v2",
		"builder prune -af --filter until=2026-11-09T00:00:00Z",
	}
	if got := f.calls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("docker calls = %q, want %q", got, want)
	}
}

// TestIntegrationCIEngineRemovalFailureIsAnExitStatus: a removal docker refuses
// exits non-zero, which the daemon reads as a failed run; and a removal docker
// performs only in part is read from its output, not from the ids passed.
func TestIntegrationCIEngineRemovalFailureIsAnExitStatus(t *testing.T) {
	f := newFakeDocker(t)
	f.failing(t, "rmi")
	f.canned(t, "rm", "c1\n")
	plan := ciEnginePlan{containers: []ciEngineContainer{{ID: "c1"}, {ID: "c2"}}, images: []ciEngineImage{{ID: "sha256:i1"}}}
	removals := ciEngineRemovals(plan)
	out, ok := f.runArgv(t, removals[0].argv...)
	if !ok {
		t.Fatalf("docker rm failed")
	}
	if missing := ciEngineMissing(removals[0], out); !strings.Contains(strings.Join(missing, " "), "c2") {
		t.Errorf("a container docker did not print is not missing: %q", missing)
	}
	if _, ok := f.runArgv(t, removals[1].argv...); ok {
		t.Fatalf("a refused image removal exited zero")
	}
}

// TestCIEngineListProgramPrintsTheEngine: the list program prints one JSON object
// with the data root's size and the three inspected lists, and changes nothing.
func TestIntegrationCIEngineListProgramPrintsTheEngine(t *testing.T) {
	f := newFakeDocker(t)
	f.canned(t, "ps", engHex64('1')+"\n")
	f.canned(t, "image-ls", engImageID('a')+"\n")
	f.canned(t, "container-inspect", `[{"Id":"`+engHex64('1')+`","Image":"`+engImageID('a')+`","Created":"`+engAgo(0)+`","State":{"Status":"running"},"Mounts":[]}]`)
	f.canned(t, "image-inspect", `[{"Id":"`+engImageID('a')+`","RepoTags":["ci-base:latest"],"Created":"`+engAgo(0)+`","RootFS":{"Layers":["sha256:l"]}}]`)

	stdout, ok := f.runProgram(t, ciEngineListScript, t.TempDir())
	if !ok {
		t.Fatalf("the list program failed on an engine with one container and image")
	}
	l, err := parseCIEngineListing(stdout)
	if err != nil {
		t.Fatalf("the listing does not parse: %v: %q", err, stdout)
	}
	if l.DataRootKiB == nil {
		t.Errorf("the listing names no data root figure: %q", stdout)
	}
	if len(l.Containers) != 1 || len(l.Images) != 1 || len(l.Volumes) != 0 {
		t.Errorf("listing has %d containers, %d images, %d volumes; want 1, 1, 0", len(l.Containers), len(l.Images), len(l.Volumes))
	}
	for _, call := range f.calls(t) {
		if strings.HasPrefix(call, "rm") || strings.HasPrefix(call, "volume rm") || strings.HasPrefix(call, "builder") {
			t.Errorf("the list program changed the engine: %q", call)
		}
	}
}

// TestCIEngineListProgramOnAnEmptyEngine: an engine with nothing in it still lists
// with the data root figure and three empty arrays, which parses.
func TestIntegrationCIEngineListProgramOnAnEmptyEngine(t *testing.T) {
	f := newFakeDocker(t)
	stdout, ok := f.runProgram(t, ciEngineListScript, t.TempDir())
	if !ok {
		t.Fatalf("the list program failed on an empty engine")
	}
	l, err := parseCIEngineListing(stdout)
	if err != nil {
		t.Fatalf("the empty listing does not parse: %v: %q", err, stdout)
	}
	if l.DataRootKiB == nil || len(l.Containers)+len(l.Images)+len(l.Volumes) != 0 {
		t.Errorf("the empty listing = %q", stdout)
	}
}

// TestIntegrationCIEnginePresenceArgvReachesDocker: the presence inspect reaches
// docker as the kind's inspect with the format and the ids, and a docker that
// exits 1 over a gone id still hands back the present ones on stdout.
func TestIntegrationCIEnginePresenceArgvReachesDocker(t *testing.T) {
	f := newFakeDocker(t)
	f.failing(t, "image-inspect")
	r := ciEngineRemoval{kind: "image"}
	_, ok := f.runArgv(t, r.presenceArgv([]string{"sha256:gone", "sha256:here"})...)
	if ok {
		t.Fatalf("an inspect over a gone id exited zero")
	}
	want := []string{"image inspect --format {{.Id}} sha256:gone sha256:here"}
	if got := f.calls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("docker calls = %q, want %q", got, want)
	}
}

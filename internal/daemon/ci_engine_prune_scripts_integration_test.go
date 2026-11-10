//go:build integration

package daemon

// The engine prune's two programs run as root inside the runner's dind container:
// they are shell text, so the only proof of what they run is running the text.
// This is the integration tier because each case starts sh, which the unit tier
// forbids. A fake docker on PATH stands in for the engine: it logs each call,
// prints the canned output named for its verb, and fails for a verb marked to
// fail (gt-i02983).

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

// TestCIEngineRemoveProgramRemovesTheNamedItems: the remove program removes the
// containers, then the images, then the volumes it was passed, prunes the build
// cache unused for 24 hours, and prints the three-figure reply with the count.
func TestIntegrationCIEngineRemoveProgramRemovesTheNamedItems(t *testing.T) {
	f := newFakeDocker(t)
	root := t.TempDir()
	stdout, ok := f.runProgram(t, ciEngineRemoveScript, root, "c1 c2", "sha256:i1", "v1 v2")
	if !ok {
		t.Fatalf("the remove program failed on a clean engine")
	}
	want := []string{
		"rm c1",
		"rm c2",
		"rmi -f sha256:i1",
		"volume rm v1",
		"volume rm v2",
		"builder prune -af --filter until=24h",
	}
	if got := f.calls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("docker calls = %q, want %q", got, want)
	}
	reply, err := parseCICacheTrimReply(stdout)
	if err != nil {
		t.Fatalf("the reply does not parse: %v: %q", err, stdout)
	}
	if reply.removed != 5 {
		t.Errorf("reply removed = %d, want 5", reply.removed)
	}
}

// TestCIEngineRemoveProgramStopsAtAFailure: a removal that fails stops the program
// before the rest, prints no reply, and so is a failed run, not a count.
func TestIntegrationCIEngineRemoveProgramStopsAtAFailure(t *testing.T) {
	f := newFakeDocker(t)
	f.failing(t, "rmi")
	stdout, ok := f.runProgram(t, ciEngineRemoveScript, t.TempDir(), "c1", "sha256:i1", "v1")
	if ok {
		t.Fatalf("a failed image removal exited zero")
	}
	if len(stdout) != 0 {
		t.Errorf("a failed removal printed a reply: %q", stdout)
	}
	for _, call := range f.calls(t) {
		if strings.HasPrefix(call, "volume rm") || strings.HasPrefix(call, "builder prune") {
			t.Errorf("the program went on after the failure: %q", call)
		}
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

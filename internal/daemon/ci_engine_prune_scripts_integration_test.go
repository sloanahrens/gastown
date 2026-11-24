//go:build integration

package daemon

// What this tier proves, and what it does not. It proves that the list program and
// the in-use re-read, the two shell texts the prune runs as root inside the
// runner's dind, run under a real sh and print what the daemon parses; that each
// removal and presence argv the daemon builds reaches a docker binary as the verb
// and the ids with nothing shell-quoted between them; and that a non-zero exit
// comes back as a non-zero exit. It does not prove docker's output shapes or flags: the fake docker prints
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
	"time"
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
	for _, sub := range []string{"bin", "out", "err", "fail"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("making %s: %v", sub, err)
		}
	}
	// The verb key is the command, with the object word for the verbs that take one.
	// The builder prune takes its until filter as a duration, as docker does: a value
	// that is not one is refused with docker's own message. out holds the stdout a
	// verb prints, err the stderr; a fail marker makes it exit 1 after both, which is
	// docker's shape for an inspect that printed the objects it found and then named
	// one that is gone.
	const script = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DOCKER_DIR/calls"
case "$1" in container|image|volume|builder) key="$1-$2" ;; *) key="$1" ;; esac
if [ "$key" = "builder-prune" ]; then
  for a in "$@"; do
    case "$a" in until=*)
      v=${a#until=}
      case "$v" in ''|*[!0-9hmsun.]*)
        echo "ERROR: \"until\" filter expects a duration (e.g., '24h'): time: unknown unit \"-\" in duration \"$v\"" >&2
        exit 1 ;;
      esac ;;
    esac
  done
fi
if [ -f "$FAKE_DOCKER_DIR/fail/$key" ]; then
  if [ -f "$FAKE_DOCKER_DIR/out/$key" ]; then cat "$FAKE_DOCKER_DIR/out/$key"; fi
  if [ -f "$FAKE_DOCKER_DIR/err/$key" ]; then cat "$FAKE_DOCKER_DIR/err/$key" >&2; else echo "fake: $key failed" >&2; fi
  exit 1
fi
if [ -f "$FAKE_DOCKER_DIR/out/$key" ]; then cat "$FAKE_DOCKER_DIR/out/$key"; fi
if [ -f "$FAKE_DOCKER_DIR/err/$key" ]; then cat "$FAKE_DOCKER_DIR/err/$key" >&2; fi
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

// partial makes a verb print its canned stdout, then stderr, and exit 1, which is
// docker's shape for an inspect that printed the objects it found and then named
// one that is gone.
func (f fakeDockerEnv) partial(t *testing.T, key, stderr string) {
	t.Helper()
	f.failing(t, key)
	if err := os.WriteFile(filepath.Join(f.dir, "err", key), []byte(stderr), 0o644); err != nil {
		t.Fatalf("writing the stderr for %s: %v", key, err)
	}
}

// noisy makes a verb print its canned stdout, then stderr, and exit zero.
func (f fakeDockerEnv) noisy(t *testing.T, key, stderr string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "err", key), []byte(stderr), 0o644); err != nil {
		t.Fatalf("writing the stderr for %s: %v", key, err)
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

// runProgramResult runs a shell program with the fake docker first on PATH and
// the given argv, returning its stdout, stderr and exit error.
func (f fakeDockerEnv) runProgramResult(t *testing.T, program string, args ...string) (stdout, stderr []byte, err error) {
	t.Helper()
	argv := append([]string{"-c", program, "program"}, args...)
	cmd := exec.Command("sh", argv...)
	cmd.Env = []string{
		"PATH=" + filepath.Join(f.dir, "bin") + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"FAKE_DOCKER_DIR=" + f.dir,
	}
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	runErr := cmd.Run()
	if runErr != nil {
		t.Logf("program exited: %v: %s", runErr, errOut.String())
	}
	return out.Bytes(), errOut.Bytes(), runErr
}

// runProgram runs a shell program with the fake docker first on PATH and the
// given argv, returning its stdout and whether it exited zero.
func (f fakeDockerEnv) runProgram(t *testing.T, program string, args ...string) ([]byte, bool) {
	t.Helper()
	stdout, _, err := f.runProgramResult(t, program, args...)
	return stdout, err == nil
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
	if _, ok := f.runArgv(t, ciEngineBuildCachePruneArgv(24*time.Hour, 10<<30)...); !ok {
		t.Fatalf("the build cache prune failed on a clean engine")
	}
	want := []string{
		"rm c1 c2",
		"rmi -f sha256:i1",
		"volume rm v1 v2",
		"builder prune -af --filter until=24h --reserved-space 10240MB",
	}
	if got := f.calls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("docker calls = %q, want %q", got, want)
	}
}

// TestIntegrationCIEngineBuildCachePruneRejectsATimestamp: the build cache prune the
// daemon builds is accepted, and a timestamp in its until filter is refused by docker
// with exit 1, so a run that sends one cannot pass.
func TestIntegrationCIEngineBuildCachePruneRejectsATimestamp(t *testing.T) {
	f := newFakeDocker(t)
	if _, ok := f.runArgv(t, ciEngineBuildCachePruneArgv(24*time.Hour, 10<<30)...); !ok {
		t.Fatalf("the build cache prune the daemon builds was refused")
	}
	timestamp := []string{"docker", "builder", "prune", "-af", "--filter", "until=2026-11-09T20:03:27-06:00"}
	if _, ok := f.runArgv(t, timestamp...); ok {
		t.Fatalf("docker's builder prune accepted a timestamp in its until filter")
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

// TestIntegrationCIEngineListToleratesAGoneObject: docker exits 1 over a container,
// image or volume CI deleted between the ls and the inspect, after printing the
// objects it did inspect. The program keeps that array on stdout, hands docker's
// stderr back and exits non-zero; the listing is usable when every stderr line is
// docker's missing-object report and the array parses.
func TestIntegrationCIEngineListToleratesAGoneObject(t *testing.T) {
	container := `[{"Id":"` + engHex64('1') + `","Image":"` + engImageID('a') + `","Created":"` + engAgo(0) + `","State":{"Status":"running"},"Mounts":[]}]`
	image := `[{"Id":"` + engImageID('a') + `","RepoTags":["ci-base:latest"],"Created":"` + engAgo(0) + `","RootFS":{"Layers":["sha256:l"]}}]`
	volume := `[{"Name":"kept","CreatedAt":"` + engAgo(0) + `"}]`
	for _, kind := range []string{"container", "image", "volume"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeDocker(t)
			f.canned(t, "ps", engHex64('1')+"\n")
			f.canned(t, "image-ls", engImageID('a')+"\n")
			f.canned(t, "volume-ls", "kept\n")
			f.canned(t, "container-inspect", container)
			f.canned(t, "image-inspect", image)
			f.canned(t, "volume-inspect", volume)
			f.partial(t, kind+"-inspect", "Error: No such "+kind+": gone\n")

			stdout, stderr, err := f.runProgramResult(t, ciEngineListScript, t.TempDir())
			if err == nil {
				t.Fatalf("the list program exited zero after docker reported a gone %s", kind)
			}
			l, listingErr := ciEngineListFromExec(stdout, stderr, err)
			if listingErr != nil {
				t.Fatalf("a listing whose only stderr is a gone %s was refused: %v: %q", kind, listingErr, stdout)
			}
			if len(l.Containers) != 1 || len(l.Images) != 1 || len(l.Volumes) != 1 {
				t.Errorf("the listing with a gone %s = %d containers, %d images, %d volumes; want 1 of each",
					kind, len(l.Containers), len(l.Images), len(l.Volumes))
			}
		})
	}
}

// TestIntegrationCIEngineListProgramKeepsAFailureLoud: an inspect that exits zero
// but writes stderr, and a run that printed no array, both fail the listing loud.
// The program hands the stderr back and ciEngineListFromExec refuses it, since the
// line is not docker's missing-object report; a refused listing prunes nothing.
func TestIntegrationCIEngineListProgramKeepsAFailureLoud(t *testing.T) {
	t.Run("a successful inspect's stderr", func(t *testing.T) {
		f := newFakeDocker(t)
		f.canned(t, "ps", engHex64('1')+"\n")
		f.canned(t, "container-inspect", `[{"Id":"`+engHex64('1')+`","Image":"`+engImageID('a')+`","Created":"`+engAgo(0)+`","State":{"Status":"running"},"Mounts":[]}]`)
		f.noisy(t, "container-inspect", "WARNING: docker could not read the config\n")

		stdout, stderr, err := f.runProgramResult(t, ciEngineListScript, t.TempDir())
		if err == nil {
			t.Fatalf("the list program exited zero after a successful inspect wrote stderr")
		}
		if _, listingErr := ciEngineListFromExec(stdout, stderr, err); listingErr == nil {
			t.Fatalf("a successful inspect's stderr was read as a usable listing: %q", stdout)
		}
	})
	t.Run("no array on stdout", func(t *testing.T) {
		f := newFakeDocker(t)
		f.failing(t, "ps")

		stdout, stderr, err := f.runProgramResult(t, ciEngineListScript, t.TempDir())
		if err == nil {
			t.Fatalf("the list program exited zero when docker ps failed")
		}
		_, listingErr := ciEngineListFromExec(stdout, stderr, err)
		if listingErr == nil {
			t.Fatalf("a run that printed no array was read as a usable listing: %q", stdout)
		}
		if !strings.Contains(listingErr.Error(), "fake: ps failed") {
			t.Errorf("the refusal does not name docker's stderr: %v", listingErr)
		}
	})
}

// TestIntegrationCIEngineInUseImagesProgramPrintsImageIDs: the in-use re-read runs
// under a real sh, asks docker for the image id of each container through container
// inspect, and prints each id on its own line for the daemon's parser.
func TestIntegrationCIEngineInUseImagesProgramPrintsImageIDs(t *testing.T) {
	f := newFakeDocker(t)
	f.canned(t, "ps", engHex64('1')+"\n"+engHex64('2')+"\n")
	f.canned(t, "container-inspect", engImageID('a')+"\n"+engImageID('b')+"\n")

	stdout, ok := f.runProgram(t, ciEngineInUseImagesScript)
	if !ok {
		t.Fatalf("the in-use program failed on an engine with two containers")
	}
	inUse, err := ciEngineInUseImageIDs(stdout)
	if err != nil {
		t.Fatalf("the in-use output does not parse: %v: %q", err, stdout)
	}
	for _, id := range []string{engImageID('a'), engImageID('b')} {
		if !inUse[id] {
			t.Errorf("the in-use output does not name %s: %q", id, stdout)
		}
	}
	want := []string{
		"ps -aq --no-trunc",
		"container inspect --format {{.Image}} " + engHex64('1') + " " + engHex64('2'),
	}
	if got := f.calls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("docker calls = %q, want %q", got, want)
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

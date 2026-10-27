package polecatimage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

const (
	seedSHA     = "0123456789abcdef0123456789abcdef01234567"
	seedOtherSH = "fedcba9876543210fedcba9876543210fedcba98"
	seedClone   = "http://forgejo:3000/town"
)

// seedHarness is the seed pass's filesystem and children: every read, write, mkdir and process is
// recorded in order, so a test pins the sequence the Job depends on without running git or go.
type seedHarness struct {
	t      *testing.T
	files  map[string][]byte
	events []string
	reply  func(SeedCmd) (string, error)
	// readErr, writeErr and mkdirErr make the matching filesystem call fail, the way a full claim
	// or a read-only mount does.
	readErr  error
	writeErr error
	mkdirErr error
}

func newSeedHarness(t *testing.T) *seedHarness {
	return &seedHarness{t: t, files: map[string][]byte{}}
}

func (h *seedHarness) ReadFile(p string) ([]byte, error) {
	if h.readErr != nil {
		return nil, h.readErr
	}
	if b, ok := h.files[p]; ok {
		h.events = append(h.events, "read: "+p)
		return b, nil
	}
	return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
}

func (h *seedHarness) WriteFile(p string, b []byte, _ os.FileMode) error {
	if h.writeErr != nil {
		return h.writeErr
	}
	h.events = append(h.events, "write: "+p)
	h.files[p] = b
	return nil
}

func (h *seedHarness) MkdirAll(p string, _ os.FileMode) error {
	if h.mkdirErr != nil {
		return h.mkdirErr
	}
	h.events = append(h.events, "mkdir: "+p)
	return nil
}

func (h *seedHarness) run(c SeedCmd) (string, error) {
	h.events = append(h.events, "run: "+strings.Join(c.Argv, " ")+" [uid "+fmt.Sprint(c.UID)+" in "+c.Dir+"]")
	if h.reply != nil {
		return h.reply(c)
	}
	return "", nil
}

// pass is a seed pass over two pins, wired to the harness.
func (h *seedHarness) pass(pins []cachevol.Pin) *SeedPass {
	return &SeedPass{
		Root:   "/cache",
		Source: "/seed",
		Pins:   pins,
		Files:  h,
		Run:    h.run,
		Logf:   func(string, ...any) {},
	}
}

func twoPins() []cachevol.Pin {
	return []cachevol.Pin{
		{Rig: "gastown", Commit: seedSHA, URL: seedClone + "/gastown.git"},
		{Rig: "devops", Commit: seedSHA, URL: seedClone + "/devops.git"},
	}
}

// TestSeedPassRunsThePlanAsTheCacheUserThenHandsOverAndMarks pins the whole sequence: the cache
// directories are created and handed to the cache user, each rig's PlanSeed steps run in its own
// checkout as that user, the claim is chowned and chmodded, and the marker is written last.
func TestSeedPassRunsThePlanAsTheCacheUserThenHandsOverAndMarks(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)
	if err := h.pass(twoPins()).Seed(); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	want := []string{
		"mkdir: /cache",
		"mkdir: /cache/gocache",
		"mkdir: /cache/gomodcache",
		"run: chown 10001:10001 /cache/gocache /cache/gomodcache [uid -1 in ]",
		"run: chown -R 10001:10001 /seed [uid -1 in ]",
	}
	for _, rig := range []string{"gastown", "devops"} {
		for _, step := range []string{"go mod download", "go build ./...", "go vet ./...", "go test -count=1 -run ^$ ./..."} {
			want = append(want, fmt.Sprintf("run: %s [uid 10001 in /seed/%s]", step, rig))
		}
	}
	want = append(want,
		"run: chown -R 10001:10001 /cache [uid -1 in ]",
		"run: chmod -R a+rwX /cache [uid -1 in ]",
		"write: /cache/.seed-pins",
	)
	if !equalEvents(h.events, want) {
		t.Errorf("seed pass ran\n%s\nwant\n%s", strings.Join(h.events, "\n"), strings.Join(want, "\n"))
	}
	if got, want := string(h.files["/cache/.seed-pins"]), cachevol.SeedMarker(twoPins()); got != want {
		t.Errorf("marker = %q, want %q", got, want)
	}
}

// TestSeedPassDoesNothingWhenTheMarkerHoldsTheSamePins: the marker is what makes an operator's
// rerun cheap, so a claim it already covers is left alone.
func TestSeedPassDoesNothingWhenTheMarkerHoldsTheSamePins(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)
	h.files["/cache/.seed-pins"] = []byte(cachevol.SeedMarker(twoPins()))
	if err := h.pass(twoPins()).Seed(); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if len(h.events) != 1 || h.events[0] != "read: /cache/.seed-pins" {
		t.Errorf("a seeded claim ran %v, want the marker read alone", h.events)
	}
}

// TestSeedPassRerunsWhenTheMarkerHoldsOtherPins: a changed commit is a different seed.
func TestSeedPassRerunsWhenTheMarkerHoldsOtherPins(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)
	other := twoPins()
	other[1].Commit = seedOtherSH
	h.files["/cache/.seed-pins"] = []byte(cachevol.SeedMarker(other))
	if err := h.pass(twoPins()).Seed(); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if got := string(h.files["/cache/.seed-pins"]); got != cachevol.SeedMarker(twoPins()) {
		t.Errorf("marker = %q, want this run's pins", got)
	}
	if !strings.Contains(strings.Join(h.events, "\n"), "go build ./...") {
		t.Errorf("a changed pin did not reseed: %v", h.events)
	}
}

// TestSeedPassStopsOnARequiredStepAndWritesNoMarker: a marker is the claim that every step ran, so
// a failed required step must not reach it, and nothing after the failure runs.
func TestSeedPassStopsOnARequiredStepAndWritesNoMarker(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)
	h.reply = func(c SeedCmd) (string, error) {
		if c.Dir == "/seed/devops" && strings.Join(c.Argv, " ") == "go build ./..." {
			return "FAIL: ./stale\n", errors.New("exit status 1")
		}
		return "", nil
	}
	err := h.pass(twoPins()).Seed()
	if err == nil {
		t.Fatal("Seed() = nil, want the failed step's error")
	}
	for _, want := range []string{"go build ./...", "/seed/devops", "FAIL: ./stale"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to carry %q", err, want)
		}
	}
	if _, ok := h.files["/cache/.seed-pins"]; ok {
		t.Error("a failed seed wrote the marker")
	}
	last := h.events[len(h.events)-1]
	if !strings.HasSuffix(last, "go build ./... [uid 10001 in /seed/devops]") {
		t.Errorf("the seed went on after the failed step: %s", last)
	}
}

// TestSeedPassLogsAndSkipsAnOptionalStep: the Dockerfile seed RUN logs a failing vet or test
// compile and carries on, and so does the Job.
func TestSeedPassLogsAndSkipsAnOptionalStep(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)
	h.reply = func(c SeedCmd) (string, error) {
		if strings.Join(c.Argv, " ") == "go vet ./..." {
			return "vet: no such package\n", errors.New("exit status 1")
		}
		return "", nil
	}
	var logged []string
	p := h.pass([]cachevol.Pin{twoPins()[0]})
	p.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	if err := p.Seed(); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "go vet ./... failed, skipped") {
		t.Errorf("logged %q, want the skipped step", logged)
	}
	if _, ok := h.files["/cache/.seed-pins"]; !ok {
		t.Error("a skipped optional step left no marker")
	}
}

// TestSeedPassRefusesARigItCannotUseAsADirectory: the rig name becomes a directory under the
// checkouts, so a name that could escape it is refused before any step runs.
func TestSeedPassRefusesARigItCannotUseAsADirectory(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)
	pins := twoPins()
	pins[0].Rig = "../etc"
	if err := h.pass(pins).Seed(); err == nil {
		t.Fatal("Seed() = nil, want the rig refused")
	}
	if len(h.events) != 0 {
		t.Errorf("a refused rig still ran %v", h.events)
	}
}

// TestSeedPassFailsClosedOnTheClaimItCannotReach drives each failure before and after the steps: a
// seed that cannot read its marker, create the cache directories, hand the claim over, or write the
// marker reports it, and none of them leaves a marker that would make the next run skip the seed.
func TestSeedPassFailsClosedOnTheClaimItCannotReach(t *testing.T) {
	t.Parallel()
	permissionDenied := &fs.PathError{Op: "open", Path: "/cache/.seed-pins", Err: fs.ErrPermission}
	tests := []struct {
		name  string
		setup func(*seedHarness)
		want  string
	}{
		{name: "a marker it cannot read", setup: func(h *seedHarness) { h.readErr = permissionDenied }, want: "reading the seed marker"},
		{name: "a cache directory it cannot make", setup: func(h *seedHarness) { h.mkdirErr = fs.ErrPermission }, want: "creating /cache"},
		{
			name: "the cache directories it cannot hand over",
			setup: func(h *seedHarness) {
				h.reply = failOn("chown 10001:10001 /cache/gocache /cache/gomodcache")
			},
			want: "chown 10001:10001",
		},
		{name: "the checkouts it cannot hand over", setup: func(h *seedHarness) { h.reply = failOn("chown -R 10001:10001 /seed") }, want: "chown -R 10001:10001 /seed"},
		{name: "the claim it cannot hand over", setup: func(h *seedHarness) { h.reply = failOn("chown -R 10001:10001 /cache") }, want: "chown -R 10001:10001 /cache"},
		{name: "the claim it cannot chmod", setup: func(h *seedHarness) { h.reply = failOn("chmod -R a+rwX /cache") }, want: "chmod -R a+rwX"},
		{name: "the marker it cannot write", setup: func(h *seedHarness) { h.writeErr = fs.ErrPermission }, want: "writing the seed marker"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSeedHarness(t)
			tc.setup(h)
			err := h.pass(twoPins()).Seed()
			if err == nil {
				t.Fatal("Seed() = nil, want the failure reported")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to carry %q", err, tc.want)
			}
			if _, ok := h.files["/cache/.seed-pins"]; ok {
				t.Error("a failed seed left a marker")
			}
		})
	}
}

// TestSeedPassRefusesNoPins: a pass with nothing to seed is a Job that was rendered wrong, so it
// fails rather than marking an empty claim as seeded.
func TestSeedPassRefusesNoPins(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)
	if err := h.pass(nil).Seed(); err == nil {
		t.Fatal("Seed() = nil, want the empty pin list refused")
	}
	if len(h.events) != 0 {
		t.Errorf("an empty pin list ran %v", h.events)
	}
}

// failOn answers a runner whose command line contains want with a failure, so a test can break one
// handover and see the seed stop there.
func failOn(want string) func(SeedCmd) (string, error) {
	return func(c SeedCmd) (string, error) {
		if strings.Contains(strings.Join(c.Argv, " "), want) {
			return "operation not permitted\n", errors.New("exit status 1")
		}
		return "", nil
	}
}

// TestCloneSeedRefusesWhatItCannotClone drives the init container's refusals: nothing to check out,
// a pin the plan refuses, a URL with a credential, and a destination it cannot create.
func TestCloneSeedRefusesWhatItCannotClone(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatalf("planting a file: %v", err)
	}
	tests := []struct {
		name string
		dest string
		pins []cachevol.Pin
	}{
		{name: "no pins", dest: t.TempDir()},
		{name: "a rig that is a path", dest: t.TempDir(), pins: []cachevol.Pin{{Rig: "../gastown", Commit: seedSHA, URL: seedClone + "/gastown.git"}}},
		{name: "a URL with userinfo", dest: t.TempDir(), pins: []cachevol.Pin{{Rig: "gastown", Commit: seedSHA, URL: "https://bot:x@forgejo:3000/town/gastown.git"}}},
		{name: "a destination it cannot create", dest: filepath.Join(notADir, "seed"), pins: twoPins()[:1]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := CloneSeed(tc.dest, tc.pins, h.run, func(string, ...any) {}); err == nil {
				t.Fatal("CloneSeed() = nil, want the pin refused")
			}
			if len(h.events) != 0 {
				t.Errorf("a refused clone still ran %v", h.events)
			}
		})
	}
}

// TestExecSeedRunRefusesAnEmptyCommand is the production runner's own guard: a seed child with no
// argv is a bug that must not start any process.
func TestExecSeedRunRefusesAnEmptyCommand(t *testing.T) {
	t.Parallel()
	if _, err := execSeedRun(SeedCmd{}); err == nil {
		t.Fatal("execSeedRun(nothing) = nil, want a refusal")
	}
}

// TestRunSeedChildReportsAFailedChild: the spawn is handed in and its child never starts, so the
// runner's own failure path is driven without running a process in a unit test.
func TestRunSeedChildReportsAFailedChild(t *testing.T) {
	t.Parallel()
	var spawned []string
	spawn := func(name string, arg ...string) *exec.Cmd {
		spawned = append(spawned, name+" "+strings.Join(arg, " "))
		c := exec.Command(name, arg...)
		c.Err = errors.New("exit status 7")
		return c
	}
	_, err := runSeedChild(spawn, SeedCmd{Dir: "/seed/gastown", Argv: []string{"go", "build", "./..."}, UID: cachevol.SeedUserID, GID: cachevol.SeedUserID})
	if err == nil {
		t.Fatal("runSeedChild() = nil, want the child's failure")
	}
	if len(spawned) != 1 || spawned[0] != "go build ./..." {
		t.Errorf("ran %v, want the seed command alone", spawned)
	}
}

// TestCloneSeedChecksEachPinOutAtItsCommit pins the init container's children: a clone of the pin's
// URL into dest/<rig>, then that commit checked out detached.
func TestCloneSeedChecksEachPinOutAtItsCommit(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)
	dest := t.TempDir()
	var log []string
	if err := CloneSeed(dest, twoPins(), h.run, func(format string, args ...any) {
		log = append(log, fmt.Sprintf(format, args...))
	}); err != nil {
		t.Fatalf("CloneSeed: %v", err)
	}
	want := []string{
		"run: git clone --no-checkout --quiet http://forgejo:3000/town/gastown.git " + dest + "/gastown [uid -1 in ]",
		"run: git -C " + dest + "/gastown checkout --quiet --detach " + seedSHA + " [uid -1 in ]",
		"run: git clone --no-checkout --quiet http://forgejo:3000/town/devops.git " + dest + "/devops [uid -1 in ]",
		"run: git -C " + dest + "/devops checkout --quiet --detach " + seedSHA + " [uid -1 in ]",
	}
	if !equalEvents(h.events, want) {
		t.Errorf("clone ran\n%s\nwant\n%s", strings.Join(h.events, "\n"), strings.Join(want, "\n"))
	}
}

// TestCloneSeedFailureNamesTheRigAndScrubsTheURL: git quotes the URL it fetched, and the Job's log
// must carry the pin's rig and commit, not a URL that could hold a credential.
func TestCloneSeedFailureNamesTheRigAndScrubsTheURL(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)
	url := seedClone + "/gastown.git"
	h.reply = func(SeedCmd) (string, error) {
		return "fatal: could not read from '" + url + "'\n", errors.New("exit status 128")
	}
	err := CloneSeed(t.TempDir(), twoPins()[:1], h.run, func(string, ...any) {})
	if err == nil {
		t.Fatal("CloneSeed() = nil, want the failed clone")
	}
	if strings.Contains(err.Error(), url) {
		t.Errorf("error quotes the clone URL: %q", err)
	}
	for _, want := range []string{"gastown", seedSHA} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

// TestSeedHelpersRefuseAnInputTheJobCannotRender: each helper answers exit 3 with its reason, so a
// Job that is applied with a bad pin list reports itself rather than seeding nothing.
func TestSeedHelpersRefuseAnInputTheJobCannotRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "seed-clone with no pins", args: []string{"seed-clone"}, want: "no --pins"},
		{name: "seed-cache with no pins", args: []string{"seed-cache"}, want: "no --pins"},
		{name: "seed-cache with a pin list that is not JSON", args: []string{"seed-cache", "--pins", "gastown"}, want: "not the seed Job's pin list"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut strings.Builder
			code := runProgram(tc.args, &out, &errOut, envMap(map[string]string{}))
			if code != 3 {
				t.Fatalf("runProgram(%v) = %d, want 3 (%s)", tc.args, code, errOut.String())
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Errorf("stderr = %q, want it to carry %q", errOut.String(), tc.want)
			}
		})
	}
}

// TestRunProgramStillRunsThePluginOnAnUnknownArgument: the seed helpers are a dispatch on the image
// binary's entry point, so an argument the plugin's own script passes on must keep starting a run.
func TestRunProgramStillRunsThePluginOnAnUnknownArgument(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	if code := runProgram([]string{"--some-plugin-flag"}, &out, &errOut, envMap(map[string]string{})); code != 1 {
		t.Errorf("runProgram(unknown argument) = %d, want the run's own status 1", code)
	}
}

// equalEvents compares two event lists, so a failure prints both in full.
func equalEvents(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

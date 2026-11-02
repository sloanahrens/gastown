package daemon

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// tierSweepInstalledBuild is the revision the runner-image tests pin as the
// installed gt's build: longer than the image tag names, so a test also holds
// the tag to its 9 characters.
const tierSweepInstalledBuild = "0123456789abcdef0123456789abcdef01234567"

// tierSweepImageRunner answers the docker commands the runner-image path
// builds, through the daemon's command runner: the look in the sweep engine,
// the save streamed from the default engine, and the load that reads it. It
// records every argv in call order and the bytes the load read, so a test sees
// what would have run without starting a process.
type tierSweepImageRunner struct {
	mu     sync.Mutex
	calls  []string
	loaded string

	checkOut, checkErrOut string
	// checkOuts, when non-empty, answers the image looks in order before
	// checkOut answers the rest.
	checkOuts           []string
	checkErr            error
	saveOut, saveErrOut string
	saveErr             error
	loadOut, loadErrOut string
	loadErr             error
}

func (f *tierSweepImageRunner) run(cmd *exec.Cmd) ([]byte, []byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(cmd.Args, " ")+" ")
	f.mu.Unlock()

	switch {
	case hasArg(cmd.Args, "image") && hasArg(cmd.Args, "ls"):
		f.mu.Lock()
		out := f.checkOut
		if len(f.checkOuts) > 0 {
			out, f.checkOuts = f.checkOuts[0], f.checkOuts[1:]
		}
		f.mu.Unlock()
		return []byte(out), []byte(f.checkErrOut), f.checkErr
	case hasArg(cmd.Args, "save"):
		if f.saveOut != "" && cmd.Stdout != nil {
			_, _ = io.WriteString(cmd.Stdout, f.saveOut)
		}
		return nil, []byte(f.saveErrOut), f.saveErr
	case hasArg(cmd.Args, "load"):
		if cmd.Stdin != nil {
			data, err := io.ReadAll(cmd.Stdin)
			f.mu.Lock()
			f.loaded = string(data)
			f.mu.Unlock()
			if err != nil {
				return nil, []byte(f.loadErrOut), err
			}
		}
		return []byte(f.loadOut), []byte(f.loadErrOut), f.loadErr
	}
	return nil, nil, nil
}

// callList returns every recorded argv, in call order.
func (f *tierSweepImageRunner) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// callsMatching returns the recorded argv holding want as one argument, in
// call order.
func (f *tierSweepImageRunner) callsMatching(want string) []string {
	var out []string
	for _, argv := range f.callList() {
		if strings.Contains(argv, " "+want+" ") {
			out = append(out, argv)
		}
	}
	return out
}

// loadRead is what the load step read from its stdin.
func (f *tierSweepImageRunner) loadRead() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaded
}

// hasArg reports whether argv holds arg as one argument.
func hasArg(argv []string, arg string) bool {
	for _, a := range argv {
		if a == arg {
			return true
		}
	}
	return false
}

// runnerImageDaemon is a sweep daemon whose runner image follows the installed
// build, with the installed revision pinned and a fake command runner in place
// of the sweep engine.
func runnerImageDaemon(t *testing.T, now time.Time, image string) (*Daemon, *tierSweepImageRunner) {
	t.Helper()
	cfg := runnerTestConfig(t)
	cfg.Image = image
	d, _ := runnerCycleDaemon(t, now, cfg, "")
	d.buildCommitFn = func() string { return tierSweepInstalledBuild }
	fake := &tierSweepImageRunner{}
	d.execCmd = fake.run
	return d, fake
}

// TestTierSweepRunnerImageDefaultsToInstalledBuild: an empty runner.image, or
// the word installed, names the polecat image at the installed gt's build
// revision, which is what the launch is handed; an explicit pin is used
// unchanged; and a build with no revision cannot name an image at all.
func TestTierSweepRunnerImageDefaultsToInstalledBuild(t *testing.T) {
	t.Parallel()

	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}
	for _, tc := range []struct {
		name  string
		image string
		want  string
	}{
		{name: "empty follows the installed build", image: "", want: "gastown-polecat:012345678"},
		{name: "installed follows the installed build", image: config.TierSweepRunnerInstalledImage, want: "gastown-polecat:012345678"},
		{name: "an explicit pin stands", image: "gastown-polecat:23017b4dd", want: "gastown-polecat:23017b4dd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, fake := runnerImageDaemon(t, now, tc.image)
			// The image is already in the sweep engine, so this test is only
			// about what the launch is handed.
			fake.checkOut = "sha256:abc\n"
			var launched sweeprunner.Spec
			d.tierSweepSeams.launch = func(_ context.Context, _ tierSweepStage, spec sweeprunner.Spec) tierSweepLaunch {
				launched = spec
				if err := sweeprunner.Write(spec.OutDir, runnerTestResult(spec, now)); err != nil {
					t.Fatalf("the fake launch writing a result: %v", err)
				}
				return tierSweepLaunch{exitCode: 0}
			}

			if res := d.tierSweepRunStage(context.Background(), t.TempDir(), st); !res.ran {
				t.Fatalf("stage result = %+v, want it run", res)
			}
			if launched.Image != tc.want {
				t.Errorf("launched image = %q, want %q", launched.Image, tc.want)
			}
		})
	}

	t.Run("no installed revision is refused", func(t *testing.T) {
		t.Parallel()
		d, _ := runnerImageDaemon(t, atHour(time.Now(), 15), "")
		d.buildCommitFn = func() string { return "" }
		var launches int
		d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
			launches++
			return tierSweepLaunch{exitCode: 0}
		}

		res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
		if !res.ran || res.reason == "" || !strings.Contains(res.reason, "build commit") {
			t.Fatalf("stage result = %+v, want it RED naming the missing build commit", res)
		}
		if launches != 0 {
			t.Errorf("launches = %d, want 0: an image no build published is never launched", launches)
		}
	})
}

// TestTierSweepLoadsMissingImageIntoSweepEngine: an image the sweep engine does
// not have is streamed into it from the default engine, save then load, before
// the launch; an image it already has causes no load.
func TestTierSweepLoadsMissingImageIntoSweepEngine(t *testing.T) {
	t.Parallel()

	t.Run("a missing image is loaded", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, fake := runnerImageDaemon(t, now, "")
		fake.saveOut = "the image tar"
		var launches int
		d.tierSweepSeams.launch = func(_ context.Context, _ tierSweepStage, spec sweeprunner.Spec) tierSweepLaunch {
			launches++
			if err := sweeprunner.Write(spec.OutDir, runnerTestResult(spec, now)); err != nil {
				t.Fatalf("the fake launch writing a result: %v", err)
			}
			return tierSweepLaunch{exitCode: 0}
		}
		st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}

		if res := d.tierSweepRunStage(context.Background(), t.TempDir(), st); !res.ran {
			t.Fatalf("stage result = %+v, want it run", res)
		}

		// One look finds the image missing; the second, under the load lock,
		// confirms no other sweep loaded it while this one waited.
		looks := fake.callsMatching("image ls")
		if len(looks) != 2 {
			t.Fatalf("image looks = %q, want the miss and the re-check under the lock", looks)
		}
		var saveAt, loadAt = -1, -1
		for i, argv := range fake.callList() {
			if saveAt < 0 && strings.Contains(argv, " save ") {
				saveAt = i
			}
			if loadAt < 0 && strings.Contains(argv, " load ") {
				loadAt = i
			}
		}
		if saveAt < 0 || loadAt < 0 || saveAt > loadAt {
			t.Fatalf("calls = %q, want the save before the load", fake.callList())
		}
		if got := fake.loadRead(); got != fake.saveOut {
			t.Errorf("the load read %q, want the save's stream %q", got, fake.saveOut)
		}
		if launches != 1 {
			t.Errorf("launches = %d, want 1 after the image reached the engine", launches)
		}
	})

	t.Run("a present image is not loaded", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, fake := runnerImageDaemon(t, now, "")
		fake.checkOut = "sha256:abc\n"
		var launches int
		d.tierSweepSeams.launch = func(_ context.Context, _ tierSweepStage, spec sweeprunner.Spec) tierSweepLaunch {
			launches++
			if err := sweeprunner.Write(spec.OutDir, runnerTestResult(spec, now)); err != nil {
				t.Fatalf("the fake launch writing a result: %v", err)
			}
			return tierSweepLaunch{exitCode: 0}
		}
		st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}

		if res := d.tierSweepRunStage(context.Background(), t.TempDir(), st); !res.ran {
			t.Fatalf("stage result = %+v, want it run", res)
		}
		if got := fake.callsMatching("save"); len(got) != 0 {
			t.Errorf("save calls = %q, want none: the image is already in the sweep engine", got)
		}
		if got := fake.callsMatching("load"); len(got) != 0 {
			t.Errorf("load calls = %q, want none: the image is already in the sweep engine", got)
		}
		if launches != 1 {
			t.Errorf("launches = %d, want 1", launches)
		}
	})

	t.Run("an image another sweep loaded while this one waited is not loaded twice", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, fake := runnerImageDaemon(t, now, "")
		// Missing on the first look, present on the look under the lock: the
		// image arrived from another sweep's load while this one waited.
		fake.checkOuts = []string{"", "sha256:abc\n"}
		var launches int
		d.tierSweepSeams.launch = func(_ context.Context, _ tierSweepStage, spec sweeprunner.Spec) tierSweepLaunch {
			launches++
			if err := sweeprunner.Write(spec.OutDir, runnerTestResult(spec, now)); err != nil {
				t.Fatalf("the fake launch writing a result: %v", err)
			}
			return tierSweepLaunch{exitCode: 0}
		}
		st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}

		if res := d.tierSweepRunStage(context.Background(), t.TempDir(), st); !res.ran {
			t.Fatalf("stage result = %+v, want it run", res)
		}
		if got := fake.callsMatching("save"); len(got) != 0 {
			t.Errorf("save calls = %q, want none: another sweep's load finished the job", got)
		}
		if launches != 1 {
			t.Errorf("launches = %d, want 1", launches)
		}
	})
}

// TestTierSweepFailedImageLoadIsRedNotStale: an image the daemon cannot confirm
// in the sweep engine, or cannot stream into it, is a RED stage naming the
// engine's error, and nothing is launched against the image that was there
// before.
func TestTierSweepFailedImageLoadIsRedNotStale(t *testing.T) {
	t.Parallel()

	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}
	for _, tc := range []struct {
		name string
		set  func(*tierSweepImageRunner)
		want string // the engine's error, as the stage log carries it
	}{
		{
			name: "the load fails",
			set: func(f *tierSweepImageRunner) {
				f.saveOut = "the image tar"
				f.loadErrOut = "no space left on device\n"
				f.loadErr = exitErr(1)
			},
			want: "no space left on device",
		},
		{
			name: "the save fails",
			set: func(f *tierSweepImageRunner) {
				f.saveErrOut = "no such image on the default engine\n"
				f.saveErr = exitErr(1)
			},
			want: "no such image on the default engine",
		},
		{
			name: "the save fails after its stream started but the load read it whole",
			set: func(f *tierSweepImageRunner) {
				f.saveOut = "the image tar"
				f.saveErrOut = "the default engine dropped the image at the end\n"
				f.saveErr = exitErr(1)
			},
			want: "the default engine dropped the image at the end",
		},
		{
			name: "the sweep engine cannot be asked",
			set: func(f *tierSweepImageRunner) {
				f.checkErrOut = "cannot connect to the sweep engine\n"
				f.checkErr = exitErr(1)
			},
			want: "cannot connect to the sweep engine",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, fake := runnerImageDaemon(t, now, "")
			tc.set(fake)
			var launches int
			d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
				launches++
				return tierSweepLaunch{exitCode: 0}
			}

			res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
			if !res.ran || res.exitCode == 0 {
				t.Fatalf("stage result = %+v, want it run and RED", res)
			}
			if res.reason == "" {
				t.Errorf("stage result = %+v, want a RED reason", res)
			}
			if !strings.Contains(res.output, tc.want) {
				t.Errorf("stage output does not name the engine's error %q:\n%s", tc.want, res.output)
			}
			if launches != 0 {
				t.Errorf("launches = %d, want 0: a failed image load never launches a run", launches)
			}
			if got := fake.callsMatching("run -d"); len(got) != 0 {
				t.Errorf("run calls = %q, want none", got)
			}
		})
	}
}

// TestTierSweepRunnerImageLoadWaitsForAnotherLoad pins the lock that keeps a
// restarted daemon or a second sweep from starting a save|load beside one that
// is already going: with the lock held, the stage is RED and nothing is saved
// or loaded.
func TestTierSweepRunnerImageLoadWaitsForAnotherLoad(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, fake := runnerImageDaemon(t, now, "")
	var launches int
	d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
		launches++
		return tierSweepLaunch{exitCode: 0}
	}

	// Hold the town's lock as another sweep's load would, then bound the
	// stage's wait so the test does not sit out the real budget.
	release, err := d.tierSweepRunnerImageLock(context.Background())
	if err != nil {
		t.Fatalf("taking the runner image lock: %v", err)
	}
	defer release()
	d.tierSweepSeams.imageBudget = 50 * time.Millisecond

	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}
	res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
	if !res.ran || res.reason == "" {
		t.Fatalf("stage result = %+v, want it RED while another load holds the lock", res)
	}
	if launches != 0 {
		t.Errorf("launches = %d, want 0", launches)
	}
	if got := fake.callsMatching("save"); len(got) != 0 {
		t.Errorf("save calls = %q, want none: another sweep holds the load lock", got)
	}
}

// TestTierSweepRunnerImageLockDirectoryRefused: a lock the daemon cannot create
// is a RED stage, never a load started without it.
func TestTierSweepRunnerImageLockDirectoryRefused(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _ := runnerImageDaemon(t, now, "")
	// A file where the lock's directory belongs makes creating it fail.
	dir := filepath.Join(constants.TownRuntimePath(d.config.TownRoot), "tier-sweep")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var launches int
	d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
		launches++
		return tierSweepLaunch{exitCode: 0}
	}

	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}
	res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
	if !res.ran || res.reason == "" || !strings.Contains(res.output, "lock directory") {
		t.Fatalf("stage result = %+v, want it RED naming the lock directory", res)
	}
	if launches != 0 {
		t.Errorf("launches = %d, want 0", launches)
	}
}

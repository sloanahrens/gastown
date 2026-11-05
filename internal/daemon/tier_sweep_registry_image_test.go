package daemon

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// testRunnerRegistry is the runner image registry the registry tests name: a
// bare host:port, as the config carries it.
const testRunnerRegistry = "localhost:5001"

// registryFake answers the registry's manifest HEAD through an *http.Client
// whose transport never opens a socket. It records each request as method and
// path, so a test sees what the daemon asked for.
type registryFake struct {
	status int
	err    error

	mu   sync.Mutex
	reqs []string
}

// RoundTrip answers with the fake's status, or fails with its error, and
// records the request.
func (f *registryFake) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req.Method+" "+req.URL.Path)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{StatusCode: f.status, Body: http.NoBody, Request: req, Header: make(http.Header)}, nil
}

// requests returns each recorded request, in call order.
func (f *registryFake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

// registryImageDaemon is a sweep daemon whose runner image follows the
// installed build, with image_registry set and the registry answering through
// reg. The engine's command runner is fake, so a test sees any engine command
// that would have run.
func registryImageDaemon(t *testing.T, now time.Time, reg *registryFake) (*Daemon, *tierSweepImageRunner) {
	t.Helper()
	d, fake := runnerImageDaemon(t, now, "")
	d.patrolConfig.Patrols.TierSweep.Runner.ImageRegistry = testRunnerRegistry
	d.tierSweepSeams.registry = &http.Client{Transport: reg}
	return d, fake
}

// registryImageLaunch records the spec a stage launches and writes a result
// for it, so the stage reads a GREEN run. launches counts the calls.
func registryImageLaunch(t *testing.T, d *Daemon, now time.Time, launched *sweeprunner.Spec, launches *int) {
	t.Helper()
	d.tierSweepSeams.launch = func(_ context.Context, _ tierSweepStage, spec sweeprunner.Spec) tierSweepLaunch {
		*launches++
		*launched = spec
		if err := sweeprunner.Write(spec.OutDir, runnerTestResult(spec, now)); err != nil {
			t.Fatalf("the fake launch writing a result: %v", err)
		}
		return tierSweepLaunch{exitCode: 0}
	}
}

// TestTierSweepEnsureImageUsesTheRegistryWhenConfigured: with image_registry set
// and the tag in the registry, the run starts on the image named inside the
// registry, the registry is asked for that tag with a HEAD, and no engine
// command runs: no image look, no save, no load, and no load lock is taken
// (gt-gg9hv.26).
func TestTierSweepEnsureImageUsesTheRegistryWhenConfigured(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	reg := &registryFake{status: http.StatusOK}
	d, fake := registryImageDaemon(t, now, reg)
	var launched sweeprunner.Spec
	var launches int
	registryImageLaunch(t, d, now, &launched, &launches)
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}

	if res := d.tierSweepRunStage(context.Background(), t.TempDir(), st); !res.ran {
		t.Fatalf("stage result = %+v, want it run", res)
	}

	wantImage := testRunnerRegistry + "/gastown-polecat:" + tierSweepInstalledBuild
	if launches != 1 || launched.Image != wantImage {
		t.Errorf("launches = %d, image = %q; want one launch on %q", launches, launched.Image, wantImage)
	}
	wantReq := "HEAD /v2/gastown-polecat/manifests/" + tierSweepInstalledBuild
	if got := reg.requests(); len(got) != 1 || got[0] != wantReq {
		t.Errorf("registry requests = %q, want exactly %q", got, wantReq)
	}
	for _, word := range []string{"image", "save", "load"} {
		if got := fake.callsMatching(word); len(got) != 0 {
			t.Errorf("%s calls = %q, want none: a registry-named image is never looked for or streamed", word, got)
		}
	}
	if _, err := os.Stat(tierSweepRunnerImageLockPath(d.config.TownRoot)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("runner image lock stat = %v, want it never created: the registry path takes no lock", err)
	}
}

// TestTierSweepEnsureImageRefusesAMissingRegistryTag: a 404 is an error that
// names the full image and the publish step, the stage is RED, nothing is
// launched and nothing is streamed (gt-gg9hv.26).
func TestTierSweepEnsureImageRefusesAMissingRegistryTag(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	reg := &registryFake{status: http.StatusNotFound}
	d, fake := registryImageDaemon(t, now, reg)
	var launched sweeprunner.Spec
	var launches int
	registryImageLaunch(t, d, now, &launched, &launches)
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}

	res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
	if !res.ran || res.exitCode == 0 || res.reason == "" {
		t.Fatalf("stage result = %+v, want it run and RED with a reason", res)
	}
	wantImage := testRunnerRegistry + "/gastown-polecat:" + tierSweepInstalledBuild
	for _, want := range []string{wantImage, "make polecat-image"} {
		if !strings.Contains(res.output, want) {
			t.Errorf("stage output = %q, want it to name %q", res.output, want)
		}
	}
	if launches != 0 {
		t.Errorf("launches = %d, want 0: a tag the registry lacks never starts a run", launches)
	}
	for _, word := range []string{"save", "load"} {
		if got := fake.callsMatching(word); len(got) != 0 {
			t.Errorf("%s calls = %q, want none: the sweep cannot publish a missing tag by streaming it", word, got)
		}
	}
}

// TestTierSweepEnsureImageRegistryErrorIsNeitherPresentNorMissing: an answer
// other than 200 or 404, and a call that fails, are each an error. The stage is
// RED and nothing launches, so neither a present image nor a missing one is
// assumed (gt-gg9hv.26).
func TestTierSweepEnsureImageRegistryErrorIsNeitherPresentNorMissing(t *testing.T) {
	t.Parallel()

	cases := map[string]*registryFake{
		"a 500 answer":  {status: http.StatusInternalServerError},
		"a failed call": {err: errors.New("dial tcp: connection refused")},
	}
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}
	for name, reg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, fake := registryImageDaemon(t, now, reg)
			var launched sweeprunner.Spec
			var launches int
			registryImageLaunch(t, d, now, &launched, &launches)

			res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
			if !res.ran || res.exitCode == 0 || res.reason == "" {
				t.Fatalf("stage result = %+v, want it run and RED with a reason", res)
			}
			if launches != 0 {
				t.Errorf("launches = %d, want 0: an unconfirmed registry tag never starts a run", launches)
			}
			for _, word := range []string{"image", "save", "load"} {
				if got := fake.callsMatching(word); len(got) != 0 {
					t.Errorf("%s calls = %q, want none", word, got)
				}
			}
		})
	}
}

// TestTierSweepEnsureImageRefusesAnImageThatIsNotAPathInTheRegistry: the tag and
// the repository reach the request path, so an image that is not named inside
// the registry, or whose repository or tag is not a shape the registry accepts,
// is refused before any request is sent (gt-gg9hv.26).
func TestTierSweepEnsureImageRefusesAnImageThatIsNotAPathInTheRegistry(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"not inside the registry":  "gastown-polecat:" + tierSweepInstalledBuild,
		"an upper-case repository": testRunnerRegistry + "/Gastown-Polecat:" + tierSweepInstalledBuild,
		"a tag with a path":        testRunnerRegistry + "/gastown-polecat:../manifests",
		"a tag with a space":       testRunnerRegistry + "/gastown-polecat:bad tag",
		"no tag":                   testRunnerRegistry + "/gastown-polecat",
	}
	for name, image := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			reg := &registryFake{status: http.StatusOK}
			d, _ := registryImageDaemon(t, now, reg)

			if err := d.tierSweepRegistryImageCheck(context.Background(), testRunnerRegistry, image); err == nil {
				t.Fatalf("check of %q = nil, want a refusal", image)
			}
			if got := reg.requests(); len(got) != 0 {
				t.Errorf("registry requests = %q, want none: a refused name never reaches the registry", got)
			}
		})
	}
}

// TestTierSweepEnsureImageRefusesARegistryThatIsNotAnAddress: a config built in
// code is not decoded, so its registry can carry an address the config check
// would refuse. The request cannot be built from it, so the stage is RED, the
// registry is never asked and nothing is launched or streamed (gt-gg9hv.26).
func TestTierSweepEnsureImageRefusesARegistryThatIsNotAnAddress(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	reg := &registryFake{status: http.StatusOK}
	d, fake := registryImageDaemon(t, now, reg)
	const badRegistry = "local%zz:5001"
	d.patrolConfig.Patrols.TierSweep.Runner.ImageRegistry = badRegistry
	var launched sweeprunner.Spec
	var launches int
	registryImageLaunch(t, d, now, &launched, &launches)
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}

	res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
	if !res.ran || res.exitCode == 0 || res.reason == "" {
		t.Fatalf("stage result = %+v, want it run and RED with a reason", res)
	}
	if want := "asking the image registry " + badRegistry; !strings.Contains(res.output, want) {
		t.Errorf("stage output = %q, want it to name %q", res.output, want)
	}
	if launches != 0 {
		t.Errorf("launches = %d, want 0: a registry that cannot be asked never starts a run", launches)
	}
	if got := reg.requests(); len(got) != 0 {
		t.Errorf("registry requests = %q, want none: the request cannot be built", got)
	}
	for _, word := range []string{"save", "load"} {
		if got := fake.callsMatching(word); len(got) != 0 {
			t.Errorf("%s calls = %q, want none", word, got)
		}
	}
}

// TestTierSweepEnsureImageWithoutARegistryIsUnchanged: with image_registry unset,
// the image is the one the engine path has always used: the image is looked for
// in the sweep engine, streamed from the default engine when missing, and
// launched under its bare name. The registry is never asked (gt-gg9hv.26).
func TestTierSweepEnsureImageWithoutARegistryIsUnchanged(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	reg := &registryFake{status: http.StatusOK}
	d, fake := runnerImageDaemon(t, now, "")
	d.tierSweepSeams.registry = &http.Client{Transport: reg}
	fake.saveOut = "the image tar"
	var launched sweeprunner.Spec
	var launches int
	registryImageLaunch(t, d, now, &launched, &launches)
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}

	if res := d.tierSweepRunStage(context.Background(), t.TempDir(), st); !res.ran {
		t.Fatalf("stage result = %+v, want it run", res)
	}

	if want := "gastown-polecat:" + tierSweepInstalledBuild; launched.Image != want {
		t.Errorf("launched image = %q, want the bare name %q", launched.Image, want)
	}
	for _, word := range []string{"image", "save", "load"} {
		if got := fake.callsMatching(word); len(got) == 0 {
			t.Errorf("%s calls = none, want the engine path to run them", word)
		}
	}
	if got := reg.requests(); len(got) != 0 {
		t.Errorf("registry requests = %q, want none without image_registry", got)
	}
}

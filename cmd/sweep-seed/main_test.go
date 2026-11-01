package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sweeprunner"
	"github.com/steveyegge/gastown/internal/testutil"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// recorder is a StepRunner that records every argv and fails the one a test names.
type recorder struct {
	argvs  [][]string
	failOn string
	// running is what the docker ps of the sweep containers prints.
	running string
}

func (r *recorder) run(argv []string, stdout, _ io.Writer) error {
	r.argvs = append(r.argvs, slices.Clone(argv))
	if slices.Contains(argv, "ps") {
		io.WriteString(stdout, r.running)
		return nil
	}
	if r.failOn != "" && strings.Contains(strings.Join(argv, " "), r.failOn) {
		return errors.New("boom")
	}
	return nil
}

func joined(argvs [][]string) []string {
	var out []string
	for _, a := range argvs {
		out = append(out, strings.Join(a, " "))
	}
	return out
}

// With no image named, the seed pulls the default images into the seed volume on
// the named context and ends by removing its temporary engine.
func TestRunSeedsTheDefaultImages(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	var stdout, stderr bytes.Buffer

	code := Run([]string{"--context", "colima-gt-control"}, &stdout, &stderr, r.run)

	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	steps := joined(r.argvs)
	if want := "docker --context colima-gt-control exec gt-sweep-seed docker pull " + sweeprunner.DefaultSeedImages[0]; !slices.Contains(steps, want) {
		t.Errorf("no step %q in %q", want, steps)
	}
	if last := steps[len(steps)-1]; last != "docker --context colima-gt-control rm -f gt-sweep-seed" {
		t.Errorf("last step = %q, want the temporary engine removed", last)
	}
	if !strings.Contains(strings.Join(steps, "\n"), "--volume "+sweeprunner.SeedVolume+":/var/lib/docker") {
		t.Errorf("no step mounts the seed volume: %q", steps)
	}
}

// The seed's defaults are the image the Dolt-backed tests start; a Dolt version
// move that forgets the seed turns this red.
func TestDefaultSeedImagesAreTheDoltTestImage(t *testing.T) {
	t.Parallel()
	if !slices.Equal(sweeprunner.DefaultSeedImages, []string{testutil.DoltDockerImage}) {
		t.Errorf("DefaultSeedImages = %q, want [%q]", sweeprunner.DefaultSeedImages, testutil.DoltDockerImage)
	}
}

// A step that fails stops the run, the temporary engine is still removed, and
// the exit is 1.
func TestRunStopsAtAFailedStepAndStillTearsDown(t *testing.T) {
	t.Parallel()
	r := &recorder{failOn: "docker pull"}
	var stdout, stderr bytes.Buffer

	code := Run([]string{"--context", "c"}, &stdout, &stderr, r.run)

	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	steps := joined(r.argvs)
	for _, s := range steps {
		if strings.Contains(s, " stop ") {
			t.Errorf("the run went on past the failed pull: %q", steps)
		}
	}
	if last := steps[len(steps)-1]; !strings.Contains(last, "rm -f gt-sweep-seed") {
		t.Errorf("last step = %q, want teardown", last)
	}
}

// A command line the command cannot use is exit 2 and nothing runs.
func TestRunRefusesWhatItCannotUse(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"no context":          {},
		"unknown argument":    {"--context", "c", "--force"},
		"context no value":    {"--context"},
		"image no value":      {"--context", "c", "--image"},
		"image reads as flag": {"--context", "c", "--image", "--rm"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &recorder{}
			var stdout, stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr, r.run); code != 2 {
				t.Errorf("exit %d, want 2 (stderr %q)", code, stderr.String())
			}
			if len(r.argvs) != 0 {
				t.Errorf("ran %q", joined(r.argvs))
			}
		})
	}
}

// A sweep container running on the context stops the seed before it writes
// anything: the runs read the seed, and a write under a running copy tears it.
func TestRunRefusesWhileASweepIsRunning(t *testing.T) {
	t.Parallel()
	r := &recorder{running: "gt-sweep-dind-gastown-1234abcd-r1\n"}
	var stdout, stderr bytes.Buffer

	code := Run([]string{"--context", "c"}, &stdout, &stderr, r.run)

	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "gt-sweep-dind-gastown-1234abcd-r1") {
		t.Errorf("stderr %q does not name the running container", stderr.String())
	}
	for _, s := range joined(r.argvs) {
		if strings.Contains(s, "volume create") || strings.Contains(s, " run ") {
			t.Errorf("a seed step ran while a sweep was running: %q", s)
		}
	}
}

// A docker ps that fails is not read as "nothing running".
func TestRunRefusesWhenTheSweepListFails(t *testing.T) {
	t.Parallel()
	r := &recorder{failOn: " ps "}
	r.running = ""
	var stdout, stderr bytes.Buffer
	run := func(argv []string, so, se io.Writer) error {
		if slices.Contains(argv, "ps") {
			return errors.New("daemon down")
		}
		return r.run(argv, so, se)
	}

	if code := Run([]string{"--context", "c"}, &stdout, &stderr, run); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if len(r.argvs) != 0 {
		t.Errorf("seed steps ran after a failed list: %q", joined(r.argvs))
	}
}

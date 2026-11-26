package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
	"github.com/steveyegge/gastown/internal/sweeprunner"
	"gopkg.in/yaml.v3"
)

// runnerVerifyApplier is the real applier over a recording cluster client,
// rendering the control values through edit when it is set. It wires the sweep
// capacity preflight, which reads a listing with room for the Job.
func runnerVerifyApplier(t *testing.T, k *recordingKubectl, edit func(*forgejorunner.Manifests)) runnerApplier {
	t.Helper()
	a := testRunnerApplier(k)
	a.sweepPreflight = sweepPreflightFake
	if edit == nil {
		return a
	}
	build := a.build
	a.build = func(v forgejorunner.Values) (forgejorunner.Manifests, error) {
		m, err := build(v)
		if err == nil {
			edit(&m)
		}
		return m, err
	}
	return a
}

// engineConfig is a rendered runner config document naming labels.
func engineConfig(t *testing.T, labels ...string) string {
	t.Helper()
	doc, err := yaml.Marshal(map[string]any{"runner": map[string]any{"labels": labels}})
	if err != nil {
		t.Fatalf("building a runner config: %v", err)
	}
	return string(doc)
}

// engineAnswers answers the applier's calls: the apply and the ready wait
// succeed, and an engine image read answers from held.
func engineAnswers(t *testing.T, held map[string]bool) func(recordedKubectlCall) (string, int, error) {
	t.Helper()
	return func(call recordedKubectlCall) (string, int, error) {
		switch call.args[0] {
		case "apply", "rollout":
			return "", 0, nil
		case "get":
			if isSweepListing(call) {
				return sweepListing("7010m"), 0, nil
			}
			return "True", 0, nil
		case "exec":
			if held[call.args[len(call.args)-1]] {
				return "", 0, nil
			}
			return "", 1, nil
		}
		return "", -1, fmt.Errorf("unexpected kubectl call %v", call.args)
	}
}

// engineCommand is the engine subcommand of an exec call.
func engineCommand(call recordedKubectlCall) string {
	if len(call.args) < 3 {
		return ""
	}
	return call.args[len(call.args)-3]
}

// TestRunnerVerifyImagesChecksEachLabelInTheEngine: a label with no registry host
// is asked of the engine sidecar in the pod the render names, and a rollout
// whose engine holds that image passes with one line per label.
func TestRunnerVerifyImagesChecksEachLabelInTheEngine(t *testing.T) {
	t.Parallel()
	k := &recordingKubectl{}
	k.answer = engineAnswers(t, map[string]bool{"ci-base:latest": true})

	lines, err := runnerVerifyApplier(t, k, nil).Verify(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(lines) != 4 {
		t.Errorf("lines = %q, want one per rendered label and the sweep capacity line", lines)
	}
	want := []string{"exec", "-n", "forgejo", "deploy/forgejo-runner", "-c", "dind", "--", "docker", "image", "inspect", "ci-base:latest"}
	var engine [][]string
	for _, c := range k.calls {
		if c.args[0] == "exec" {
			engine = append(engine, c.args)
		}
	}
	if len(engine) != 1 || !slices.Equal(engine[0], want) {
		t.Errorf("engine calls = %v, want the one engine read %v", engine, want)
	}
}

// TestRunnerVerifyImagesSkipsARegistryImageTheEngineLacks: a registry-host image
// is pulled on demand, so it is not checked: an engine that lacks it passes, it
// is never reported missing, and the engine is not asked about it.
func TestRunnerVerifyImagesSkipsARegistryImageTheEngineLacks(t *testing.T) {
	t.Parallel()
	k := &recordingKubectl{}
	k.answer = engineAnswers(t, map[string]bool{"ci-base:latest": true})

	lines, err := runnerVerifyApplier(t, k, nil).Verify(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if want := "label node: image data.forgejo.org/oci/node:20.19 is pulled on demand from data.forgejo.org, not checked"; !slices.Contains(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	for _, c := range k.calls {
		if strings.Contains(strings.Join(c.args, " "), "data.forgejo.org") {
			t.Errorf("the engine was asked about a registry-host image: %v", c.args)
		}
	}
}

// TestRunnerVerifyImagesFailsALabelTheEngineLacks: an image with no registry host
// can come only from the engine, so its absence fails the verify, naming the
// label, the image and the engine.
func TestRunnerVerifyImagesFailsALabelTheEngineLacks(t *testing.T) {
	t.Parallel()
	k := &recordingKubectl{}
	k.answer = engineAnswers(t, map[string]bool{})

	_, err := runnerVerifyApplier(t, k, nil).Verify(context.Background())
	if err == nil {
		t.Fatal("verify passed with ci-base:latest absent from the engine")
	}
	for _, want := range []string{"ci-base", "ci-base:latest", forgejorunner.DindImage} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestRunnerVerifyImagesFailsWhenAReadFails: an engine read that could not run is
// an error, never a pass that the image is present.
func TestRunnerVerifyImagesFailsWhenAReadFails(t *testing.T) {
	t.Parallel()
	k := &recordingKubectl{}
	answer := engineAnswers(t, map[string]bool{"ci-base:latest": true})
	k.answer = func(call recordedKubectlCall) (string, int, error) {
		if engineCommand(call) == "image" {
			return "", 0, errors.New("exec stream broke")
		}
		return answer(call)
	}

	_, err := runnerVerifyApplier(t, k, nil).Verify(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exec stream broke") {
		t.Fatalf("error = %v, want the failed read's error", err)
	}
}

// TestRunnerVerifyImagesRefusesAnUnreadableRender: a render or a seam the check
// cannot read is refused, never checked as if it named nothing.
func TestRunnerVerifyImagesRefusesAnUnreadableRender(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(*forgejorunner.Manifests)
		take func(*runnerApplier)
		want string
	}{
		{name: "no cluster client", take: func(a *runnerApplier) { a.kubectl = nil }, want: "no cluster client"},
		{name: "values that do not read", take: func(a *runnerApplier) {
			a.values = func() (forgejorunner.Values, error) { return forgejorunner.Values{}, errors.New("no values") }
		}, want: "no values"},
		{name: "two config keys", edit: func(m *forgejorunner.Manifests) { m.ConfigMap.Data["extra"] = "x" }, want: "2 data keys"},
		{name: "a config that does not parse", edit: func(m *forgejorunner.Manifests) {
			m.ConfigMap.Data["config.yml"] = "runner: [\n"
		}, want: "reading the rendered runner config"},
		{name: "a config naming no labels", edit: func(m *forgejorunner.Manifests) {
			m.ConfigMap.Data["config.yml"] = engineConfig(t)
		}, want: "names no labels"},
		{name: "a label with no image", edit: func(m *forgejorunner.Manifests) {
			m.ConfigMap.Data["config.yml"] = engineConfig(t, "ci-base")
		}, want: "names no image"},
		{name: "a label that names no docker image", edit: func(m *forgejorunner.Manifests) {
			m.ConfigMap.Data["config.yml"] = engineConfig(t, "ci-base:pull://ci-base")
		}, want: "names no docker:// image"},
		{name: "a Deployment in no namespace", edit: func(m *forgejorunner.Manifests) {
			m.Deployment.Metadata.Namespace = ""
		}, want: "cannot be reached"},
		{name: "a Deployment with no engine", edit: func(m *forgejorunner.Manifests) {
			m.Deployment.Spec.Template.Spec.InitContainers = nil
		}, want: "no engine sidecar image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := &recordingKubectl{answer: engineAnswers(t, map[string]bool{})}
			a := runnerVerifyApplier(t, k, tc.edit)
			if tc.take != nil {
				tc.take(&a)
			}
			if _, err := a.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestRunnerVerifyImagesFailTheRollAndKeepTheHold: the roll runs the applier's
// verify after the ready wait, and a failing check leaves the landing hold taken
// for the overseer (gt-ie1tau).
func TestRunnerVerifyImagesFailTheRollAndKeepTheHold(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	k := &recordingKubectl{}
	k.answer = engineAnswers(t, map[string]bool{})
	f.deps.apply = runnerApplier{
		kubectl: k.run, values: forgejorunner.ControlValues, build: forgejorunner.Build,
		encode: forgejorunner.YAML, rolloutWait: time.Minute, out: io.Discard,
	}

	err := runTownRunnerRoll(f.deps)

	if err == nil || !strings.Contains(err.Error(), "verifying the runner") || !strings.Contains(err.Error(), "ci-base:latest") {
		t.Fatalf("error = %v, want the roll to fail naming the missing image", err)
	}
	if !f.store.present {
		t.Error("the landing hold was released after a failing verify")
	}
}

// TestRunnerVerifyImagesAddsNoChecksForAnApplierWithoutThem: a runner target
// whose applier has no image checks adds none to the roll's verify, so the roll
// runs its other steps (the Verifier is optional).
func TestRunnerVerifyImagesAddsNoChecksForAnApplierWithoutThem(t *testing.T) {
	t.Parallel()
	type noChecksApplier struct{ rollApplier }

	lines, err := runnerTarget{applier: noChecksApplier{}}.Verify(context.Background())
	if err != nil || lines != nil {
		t.Fatalf("verify = %q, %v; want no checks and no error", lines, err)
	}
}

// sweepListing is the node listing the sweep preflight reads: one Ready node with
// cpu allocatable and nothing requested on it.
func sweepListing(cpu string) string {
	return `{"items":[{"kind":"Node","metadata":{"name":"node-a"},"status":{"allocatable":{"cpu":"` + cpu + `","memory":"16Gi"},"conditions":[{"type":"Ready","status":"True"}]}}]}`
}

// isSweepListing is the sweep preflight's one node and pod read.
func isSweepListing(call recordedKubectlCall) bool {
	return len(call.args) > 1 && call.args[0] == "get" && call.args[1] == "nodes,pods"
}

// listingAnswers answers the sweep's listing read with listing, and every other
// call with answer.
func listingAnswers(answer func(recordedKubectlCall) (string, int, error), listing string) func(recordedKubectlCall) (string, int, error) {
	return func(call recordedKubectlCall) (string, int, error) {
		if isSweepListing(call) {
			return listing, 0, nil
		}
		return answer(call)
	}
}

// sweepPreflightFake is the sweep preflight a test wires: its Step is the sweep's
// one listing call, and its Read decides the listing against a 4200m Job with no
// tolerations, through the same DecideCapacity the sweep's Read decides with.
func sweepPreflightFake() *sweeprunner.Preflight {
	job := sweeprunner.Amount{CPU: 4200, Memory: 1 << 30}
	return &sweeprunner.Preflight{
		Step: sweeprunner.Step{Argv: []string{"kubectl", "get", "nodes,pods", "-A", "-o", "json"}},
		Read: func(stdout []byte) (sweeprunner.CapacityReport, error) {
			nodes, err := sweeprunner.ParseCapacity(stdout)
			if err != nil {
				return sweeprunner.CapacityReport{}, err
			}
			return sweeprunner.DecideCapacity(nodes, job, nil)
		},
	}
}

// TestRunnerVerifySweepFitsPassesWithRoom: a node with room for the Job and its
// headroom passes, and the verify reports the capacity line it read.
func TestRunnerVerifySweepFitsPassesWithRoom(t *testing.T) {
	t.Parallel()
	k := &recordingKubectl{answer: engineAnswers(t, map[string]bool{"ci-base:latest": true})}

	lines, err := runnerVerifyApplier(t, k, nil).Verify(context.Background())
	if err != nil {
		t.Fatalf("verify with 7010m free: %v", err)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "tier sweep capacity: node node-a") }) {
		t.Errorf("lines = %q, want the sweep capacity line", lines)
	}
}

// TestRunnerVerifySweepFitsDeclinesWithoutRoom: 3010m free for a 4200m Job fails
// the verify with the report's own decline line, and no line passes.
func TestRunnerVerifySweepFitsDeclinesWithoutRoom(t *testing.T) {
	t.Parallel()
	answer := engineAnswers(t, map[string]bool{"ci-base:latest": true})
	k := &recordingKubectl{answer: listingAnswers(answer, sweepListing("3010m"))}

	lines, err := runnerVerifyApplier(t, k, nil).Verify(context.Background())
	if err == nil {
		t.Fatalf("verify passed with 3010m free for a 4200m Job: %q", lines)
	}
	for _, want := range []string{"node node-a: free 3010m", "job needs 4200m", "headroom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry the decline line, want %q", err, want)
		}
	}
	if len(lines) != 0 {
		t.Errorf("lines = %q, want none on a decline", lines)
	}
}

// TestRunnerVerifySweepFitsKeepsTheHeadroom: the Job's own request is not enough;
// the node must keep the 2000m headroom free beside it, so 6100m declines and
// 6200m passes.
func TestRunnerVerifySweepFitsKeepsTheHeadroom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		free    string
		decline bool
	}{
		{free: "6100m", decline: true},
		{free: "6200m", decline: false},
	} {
		t.Run(tc.free, func(t *testing.T) {
			t.Parallel()
			answer := engineAnswers(t, map[string]bool{"ci-base:latest": true})
			k := &recordingKubectl{answer: listingAnswers(answer, sweepListing(tc.free))}

			_, err := runnerVerifyApplier(t, k, nil).Verify(context.Background())
			if tc.decline && err == nil {
				t.Errorf("verify passed with %s free, the Job's request and headroom short of its room", tc.free)
			}
			if !tc.decline && err != nil {
				t.Errorf("verify with %s free: %v", tc.free, err)
			}
		})
	}
}

// TestRunnerVerifySweepFitsRefusesWhatItCannotCheck: a sweep preflight that is
// unset, names no way to read the cluster, or whose read cannot run, exits
// nonzero, or does not parse is an error, never a pass. The error names the
// cause and never echoes the listing's output.
func TestRunnerVerifySweepFitsRefusesWhatItCannotCheck(t *testing.T) {
	t.Parallel()
	withArgv := func(argv ...string) func(*runnerApplier) {
		return func(a *runnerApplier) {
			p := sweepPreflightFake()
			p.Step.Argv = argv
			a.sweepPreflight = func() *sweeprunner.Preflight { return p }
		}
	}
	for _, tc := range []struct {
		name   string
		take   func(*runnerApplier)
		stdout string
		exit   int
		runErr error
		want   string
		notIn  string
	}{
		{name: "an unset seam", take: func(a *runnerApplier) { a.sweepPreflight = nil }, want: "not wired"},
		{name: "a seam that returns no preflight", take: func(a *runnerApplier) {
			a.sweepPreflight = func() *sweeprunner.Preflight { return nil }
		}, want: "names no listing reader"},
		{name: "a preflight with no Read", take: func(a *runnerApplier) {
			p := sweepPreflightFake()
			p.Read = nil
			a.sweepPreflight = func() *sweeprunner.Preflight { return p }
		}, want: "names no listing reader"},
		{name: "a Step with no command", take: withArgv(), want: "names no command"},
		{name: "a Step that is not kubectl", take: withArgv("helm", "get", "nodes,pods"), want: `not kubectl`},
		{name: "a listing read that cannot run", runErr: errors.New("exec stream broke"), want: "could not run its cluster read"},
		{name: "a listing read that exits nonzero", stdout: "forbidden: token-123", exit: 1, want: "exited 1", notIn: "token-123"},
		{name: "a listing that does not parse", stdout: "not json", want: "could not read the node listing"},
		{name: "a listing that names no node", stdout: `{"items":[]}`, want: "names no node"},
		{name: "a listing whose only node is not Ready", stdout: `{"items":[{"kind":"Node","metadata":{"name":"node-a"},"status":{"allocatable":{"cpu":"7010m","memory":"16Gi"},"conditions":[{"type":"Ready","status":"False"}]}}]}`, want: "no node the Job may be placed on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answer := engineAnswers(t, map[string]bool{"ci-base:latest": true})
			k := &recordingKubectl{answer: func(call recordedKubectlCall) (string, int, error) {
				if isSweepListing(call) {
					return tc.stdout, tc.exit, tc.runErr
				}
				return answer(call)
			}}
			a := runnerVerifyApplier(t, k, nil)
			if tc.take != nil {
				tc.take(&a)
			}

			lines, err := a.Verify(context.Background())
			if err == nil {
				t.Fatalf("verify passed, lines = %q; want an error naming %q", lines, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			if tc.notIn != "" && strings.Contains(err.Error(), tc.notIn) {
				t.Errorf("error %q echoes the listing's output %q", err, tc.notIn)
			}
		})
	}
}

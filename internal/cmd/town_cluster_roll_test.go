package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	k8s "github.com/steveyegge/gastown/deploy/k8s"
	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/runnerroll"
	"github.com/steveyegge/gastown/internal/update"
)

// clusterRollFixture is a cluster roll with every seam faked: an in-memory
// landing hold, quiet waits, a recording cluster client and a render over a
// temporary town. A test changes the field it exercises.
type clusterRollFixture struct {
	events  *rollEvents
	store   *rollStore
	kubectl *recordingKubectl
	signals *fakeRollSignals
	out     *bytes.Buffer
	deps    clusterRollDeps
}

// newClusterRollFixture builds the fixture over render, answering every cluster
// call through answer.
func newClusterRollFixture(t *testing.T, render clusterRenderDeps, answer update.RunFunc) *clusterRollFixture {
	t.Helper()
	ev := &rollEvents{}
	ctx, cancel := context.WithCancel(context.Background())
	signals := &fakeRollSignals{ctx: ctx, cancel: cancel, stopCh: make(chan struct{})}
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	f := &clusterRollFixture{
		events: ev,
		store:  &rollStore{events: ev},
		kubectl: &recordingKubectl{answer: func(call recordedKubectlCall) (string, int, error) {
			return answer(context.Background(), "", nil, call.name, call.args...)
		}},
		signals: signals,
		out:     &bytes.Buffer{},
	}
	f.deps = clusterRollDeps{
		apply: &clusterRollApplier{
			render:      render,
			kubectl:     f.kubectl.run,
			rolloutWait: 10 * time.Minute,
			out:         f.out,
		},
		store:    f.store,
		holdPath: filepath.Join("/town", runnerroll.LandingHoldName),
		landing: func(context.Context) (string, int, bool, error) {
			ev.add("landing")
			return runnerroll.RollPause, 0, true, nil
		},
		runs: func(context.Context) ([]runnerroll.WorkflowRun, error) {
			ev.add("runs")
			return nil, nil
		},
		clock:       &rollClock{now: t0},
		now:         func() time.Time { return t0 },
		signals:     signals.source,
		landingWait: 10 * time.Minute,
		runsWait:    20 * time.Minute,
		rolloutWait: 10 * time.Minute,
		out:         f.out,
	}
	return f
}

// clusterRollAnswering answers the cluster calls a roll makes beyond the diff's
// reads: an apply succeeds, a rollout status succeeds and a ready read answers
// True. The Deployment drift reads go to drift, which the drift tests' own
// answering supplies.
func clusterRollAnswering(t *testing.T, drift update.RunFunc) update.RunFunc {
	t.Helper()
	return func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		if name != "kubectl" {
			t.Fatalf("ran %q, want kubectl", name)
		}
		switch {
		case len(args) > 0 && args[0] == "apply":
			return "", 0, nil
		case len(args) > 1 && args[0] == "rollout" && args[1] == "status":
			return "", 0, nil
		case len(args) > 1 && args[0] == "get" && args[1] == "deployment":
			return "True", 0, nil
		}
		return drift(ctx, dir, env, name, args...)
	}
}

// driftGatewayProbeMoved moves the live bd gateway's readiness probe away from
// the render's, the hand patch a render change reaches the cluster to undo.
func driftGatewayProbeMoved(t *testing.T, live map[string]any) {
	t.Helper()
	if live["metadata"].(map[string]any)["name"] != k8sdaemon.BdGatewayName {
		return
	}
	probe := driftObjectAt(driftContainers(live)[0].(map[string]any), "readinessProbe")
	probe["initialDelaySeconds"] = float64(10)
	probe["periodSeconds"] = float64(15)
}

// clusterRollFourDeployments is the fixture's render over the town whose flags
// render the dashboard, trackerd and the bd gateway beside the daemon.
func clusterRollFourDeployments(t *testing.T) *clusterRollFixture {
	t.Helper()
	d, _, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	return newClusterRollFixture(t, d.render, clusterRollAnswering(t, driftAnswering(t, m, driftGatewayProbeMoved)))
}

// appliedObjects returns the "applying kind/name" lines the roll printed.
func appliedObjects(out *bytes.Buffer) []string {
	var applied []string
	for _, line := range driftLines(out) {
		if name, ok := strings.CutPrefix(line, "applying "); ok {
			applied = append(applied, name)
		}
	}
	return applied
}

// appliedDeployment decodes the Deployment the roll handed kubectl under name.
func appliedDeployment(t *testing.T, k *recordingKubectl, name string) k8sdaemon.Deployment {
	t.Helper()
	for _, call := range k.calls {
		if len(call.args) == 0 || call.args[0] != "apply" {
			continue
		}
		var deployment k8sdaemon.Deployment
		if err := yaml.Unmarshal([]byte(call.stdin), &deployment); err != nil {
			continue
		}
		if deployment.Kind == "Deployment" && deployment.Metadata.Name == name {
			return deployment
		}
	}
	t.Fatalf("no apply call carried deployment/%s", name)
	return k8sdaemon.Deployment{}
}

// TestTownClusterRollAppliesTheDiffAndWaits: a gateway probe that differs from
// the render applies the workload kinds in the render's order and waits for each
// Deployment's rollout and Available condition, all under the landing hold.
func TestTownClusterRollAppliesTheDiffAndWaits(t *testing.T) {
	t.Parallel()
	f := clusterRollFourDeployments(t)

	if err := runTownClusterRoll(f.deps); err != nil {
		t.Fatalf("roll: %v\nprinted:\n%s", err, f.out.String())
	}

	wantApplied := []string{
		"persistentvolumeclaim/town-state",
		"persistentvolumeclaim/landing-work",
		"persistentvolumeclaim/dolt-backup",
		"persistentvolumeclaim/bd-gateway-workspace",
		"persistentvolumeclaim/bd-gateway-log",
		"serviceaccount/daemon",
		"service/forgejo",
		"service/dashboard",
		"service/trackerd",
		"service/bd-gateway",
		"service/bd-gateway-push",
		"deployment/daemon",
		"deployment/dashboard",
		"deployment/trackerd",
		"deployment/bd-gateway",
	}
	if got := appliedObjects(f.out); !slices.Equal(got, wantApplied) {
		t.Errorf("applied = %v, want %v", got, wantApplied)
	}
	if !slices.Contains(f.events.calls, "hold:create") {
		t.Fatalf("calls = %v, want the hold taken before the apply", f.events.calls)
	}
	if f.store.present {
		t.Error("the landing hold is still in place after a clean roll")
	}
	want := []string{"hold:create", "landing", "runs", "hold:remove"}
	if !slices.Equal(f.events.calls, want) {
		t.Errorf("calls = %v, want %v", f.events.calls, want)
	}
	for _, name := range []string{"daemon", "dashboard", "trackerd", "bd-gateway"} {
		resource := "deployment/" + name
		wait := "waiting for " + resource + " to roll out"
		if !strings.Contains(f.out.String(), wait) {
			t.Errorf("the roll never waited for %s; printed:\n%s", resource, f.out.String())
		}
		if !strings.Contains(f.out.String(), resource+" is ready") {
			t.Errorf("the roll never read %s's readiness; printed:\n%s", resource, f.out.String())
		}
	}
	if !strings.Contains(f.out.String(), "deployment/bd-gateway: ") {
		t.Errorf("the roll never printed the gateway's difference; printed:\n%s", f.out.String())
	}
}

// TestTownClusterRollAppliesTheRenderedDeployment: the object handed kubectl is
// the render's own Deployment, not a re-rendered or edited copy.
func TestTownClusterRollAppliesTheRenderedDeployment(t *testing.T) {
	t.Parallel()
	d, _, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	f := newClusterRollFixture(t, d.render, clusterRollAnswering(t, driftAnswering(t, m, driftGatewayProbeMoved)))

	if err := runTownClusterRoll(f.deps); err != nil {
		t.Fatalf("roll: %v", err)
	}
	if got := appliedDeployment(t, f.kubectl, k8sdaemon.BdGatewayName); !reflect.DeepEqual(got, m.BdGatewayDeployment) {
		t.Errorf("applied gateway differs from the rendered one")
	}
}

// TestTownClusterRollEmptyDiffTakesNoHold: a cluster that already matches the
// render is never restarted, so no hold is taken and nothing is applied.
func TestTownClusterRollEmptyDiffTakesNoHold(t *testing.T) {
	t.Parallel()
	d, _, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	f := newClusterRollFixture(t, d.render, clusterRollAnswering(t, driftAnswering(t, m, nil)))

	if err := runTownClusterRoll(f.deps); err != nil {
		t.Fatalf("roll: %v\nprinted:\n%s", err, f.out.String())
	}
	if !strings.Contains(f.out.String(), "no differences") {
		t.Errorf("the roll did not say the cluster matches; printed:\n%s", f.out.String())
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want no hold taken and no wait", f.events.calls)
	}
	for _, call := range f.kubectl.calls {
		if len(call.args) > 0 && call.args[0] != "get" {
			t.Errorf("ran kubectl %v, want only the diff's reads", call.args)
		}
	}
	if got := appliedObjects(f.out); len(got) != 0 {
		t.Errorf("applied = %v, want nothing", got)
	}
}

// TestTownClusterRollRefusesStaticDocuments: a static document in the render is
// refused by kind and name before the hold is taken and before any cluster read.
func TestTownClusterRollRefusesStaticDocuments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		object  k8s.StaticObject
		wantErr string
	}{
		{
			name:    "a static Secret",
			object:  k8s.StaticObject{Kind: "Secret", Name: "gt-daemon-keys"},
			wantErr: `Secret "gt-daemon-keys"`,
		},
		{
			name:    "a static admission policy",
			object:  k8s.StaticObject{Kind: "ValidatingAdmissionPolicy", Name: "bd-gateway-service"},
			wantErr: `ValidatingAdmissionPolicy "bd-gateway-service"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _ := driftTestSeams(t, nil)
			render := d.render
			build := render.build
			render.build = func(v k8sdaemon.Values) (k8sdaemon.Manifests, error) {
				m, err := build(v)
				if err != nil {
					return m, err
				}
				m.Static = []k8s.StaticObject{tc.object}
				return m, nil
			}
			f := newClusterRollFixture(t, render, func(context.Context, string, []string, string, ...string) (string, int, error) {
				t.Error("read the cluster before refusing a static document")
				return "", 0, nil
			})

			err := runTownClusterRoll(f.deps)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one naming %s", err, tc.wantErr)
			}
			if len(f.events.calls) != 0 {
				t.Errorf("calls = %v, want no hold taken", f.events.calls)
			}
			if len(f.kubectl.calls) != 0 {
				t.Errorf("read the cluster %d time(s) before refusing", len(f.kubectl.calls))
			}
		})
	}
}

// TestTownClusterRollDryRunPrintsTheDiffAndWritesNothing: --dry-run prints the
// steps and the diff lines, takes no hold and makes no write call.
func TestTownClusterRollDryRunPrintsTheDiffAndWritesNothing(t *testing.T) {
	t.Parallel()
	f := clusterRollFourDeployments(t)
	f.deps.dryRun = true

	if err := runTownClusterRoll(f.deps); err != nil {
		t.Fatalf("dry run: %v\nprinted:\n%s", err, f.out.String())
	}
	for _, step := range []string{"step 1/5:", "step 5/5:"} {
		if !strings.Contains(f.out.String(), step) {
			t.Errorf("the dry run did not print %q; printed:\n%s", step, f.out.String())
		}
	}
	if !strings.Contains(f.out.String(), "deployment/bd-gateway: ") {
		t.Errorf("the dry run did not print the gateway's diff line; printed:\n%s", f.out.String())
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want no hold taken", f.events.calls)
	}
	for _, call := range f.kubectl.calls {
		if len(call.args) == 0 || call.args[0] != "get" {
			t.Errorf("ran kubectl %v, want only the diff's reads", call.args)
		}
	}
}

// TestTownClusterRollFailingApplyReleasesTheHold: an apply kubectl refuses ends
// the roll non-zero and gives the landing hold back.
func TestTownClusterRollFailingApplyReleasesTheHold(t *testing.T) {
	t.Parallel()
	d, _, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	answer := clusterRollAnswering(t, driftAnswering(t, m, driftGatewayProbeMoved))
	f := newClusterRollFixture(t, d.render, func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		if len(args) > 0 && args[0] == "apply" {
			return "", 1, nil
		}
		return answer(ctx, dir, env, name, args...)
	})

	err := runTownClusterRoll(f.deps)
	if err == nil || !strings.Contains(err.Error(), "kubectl exited 1") {
		t.Fatalf("err = %v, want a failing apply", err)
	}
	if f.store.present {
		t.Error("the landing hold is still in place after a failed apply")
	}
	if !slices.Contains(f.events.calls, "hold:remove") {
		t.Errorf("calls = %v, want the hold released", f.events.calls)
	}
}

// TestTownClusterRollWaitFailureReleasesTheHold: each way a Deployment's rollout or
// readiness check can fail ends the roll non-zero and gives the landing hold back,
// so a rollout kubectl could not confirm is never reported as ready.
func TestTownClusterRollWaitFailureReleasesTheHold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		answer  func(call recordedKubectlCall) (string, int, error)
		wantErr string
	}{
		{
			name: "the rollout cannot start",
			answer: func(call recordedKubectlCall) (string, int, error) {
				return "", 0, errors.New("exec: kubectl not found")
			},
			wantErr: "exec: kubectl not found",
		},
		{
			name: "the rollout does not finish",
			answer: func(call recordedKubectlCall) (string, int, error) {
				return "", 1, nil
			},
			wantErr: "did not complete its rollout within",
		},
		{
			name: "the readiness cannot be read",
			answer: func(call recordedKubectlCall) (string, int, error) {
				if call.args[0] == "get" {
					return "", 0, errors.New("exec: kubectl not found")
				}
				return "", 0, nil
			},
			wantErr: "exec: kubectl not found",
		},
		{
			name: "the readiness read fails",
			answer: func(call recordedKubectlCall) (string, int, error) {
				if call.args[0] == "get" {
					return "", 1, nil
				}
				return "", 0, nil
			},
			wantErr: "reading the readiness of",
		},
		{
			name: "the Deployment is not Available",
			answer: func(call recordedKubectlCall) (string, int, error) {
				if call.args[0] == "get" {
					return "False", 0, nil
				}
				return "", 0, nil
			},
			wantErr: `is not ready: its Available condition is "False"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, town := driftFourDeploymentSeams(t)
			m := driftTestRendered(t, d, driftTestFlags(t, town))
			drift := driftAnswering(t, m, driftGatewayProbeMoved)
			f := newClusterRollFixture(t, d.render, func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
				switch {
				case len(args) > 0 && args[0] == "apply":
					return "", 0, nil
				case len(args) > 1 && args[0] == "rollout":
					return tc.answer(recordedKubectlCall{name: name, args: args})
				case len(args) > 1 && args[0] == "get" && args[1] == "deployment":
					return tc.answer(recordedKubectlCall{name: name, args: args})
				}
				return drift(ctx, dir, env, name, args...)
			})

			err := runTownClusterRoll(f.deps)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if f.store.present {
				t.Error("the landing hold is still in place after a failed wait")
			}
			if !slices.Contains(f.events.calls, "hold:remove") {
				t.Errorf("calls = %v, want the hold released", f.events.calls)
			}
		})
	}
}

// TestTownClusterRollUnreadableCompareTakesNoHold: a diff the cluster cannot
// answer stops the roll before the hold is taken, since an unread cluster is not
// one in step.
func TestTownClusterRollUnreadableCompareTakesNoHold(t *testing.T) {
	t.Parallel()
	d, _, _ := driftFourDeploymentSeams(t)
	f := newClusterRollFixture(t, d.render, func(context.Context, string, []string, string, ...string) (string, int, error) {
		return "", 1, errors.New("kubectl could not reach the cluster")
	})

	err := runTownClusterRoll(f.deps)
	if err == nil || !strings.Contains(err.Error(), "kubectl could not reach the cluster") {
		t.Fatalf("err = %v, want the unreadable compare", err)
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want no hold taken", f.events.calls)
	}
}

// TestTownClusterRollRefusesWithoutAWiredApplier: a roll with no applier, or one
// missing a seam, refuses before it takes the hold, dry run included.
func TestTownClusterRollRefusesWithoutAWiredApplier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		applier *clusterRollApplier
		dryRun  bool
		wantErr string
	}{
		{name: "no applier", wantErr: "no applier"},
		{name: "no applier, dry run", dryRun: true, wantErr: "no applier"},
		{name: "no cluster client", applier: &clusterRollApplier{rolloutWait: time.Minute}, wantErr: "no cluster client"},
		{name: "no render seams", applier: &clusterRollApplier{kubectl: func(context.Context, string, []string, string, ...string) (string, int, error) {
			return "", 0, nil
		}, rolloutWait: time.Minute}, wantErr: "no render seams"},
		{name: "no inputs", applier: &clusterRollApplier{
			kubectl: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "", 0, nil
			},
			render: clusterRenderDeps{
				findTown: func() (string, error) { return "/town", nil },
				build:    k8sdaemon.Build,
				encode:   k8sdaemon.YAML,
			},
			rolloutWait: time.Minute,
		}, wantErr: "no render input seams"},
		{name: "no rollout wait", applier: &clusterRollApplier{
			kubectl: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "", 0, nil
			},
			render: clusterRenderDepsAt(t, t.TempDir(), io.Discard),
		}, wantErr: "the rollout wait must be longer than zero"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := clusterRollDeps{apply: tc.applier, dryRun: tc.dryRun, out: io.Discard}
			err := runTownClusterRoll(d)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// TestTownClusterRollWithoutRecordedFlagsRefuses: a town where no live render
// recorded its flags has no render to apply, so the roll names the file to
// produce rather than applying nothing.
func TestTownClusterRollWithoutRecordedFlagsRefuses(t *testing.T) {
	t.Parallel()
	f := newClusterRollFixture(t, clusterRenderDepsAt(t, t.TempDir(), io.Discard), func(context.Context, string, []string, string, ...string) (string, int, error) {
		t.Error("read the cluster before refusing a town with no recorded flags")
		return "", 0, nil
	})

	err := runTownClusterRoll(f.deps)
	if err == nil || !strings.Contains(err.Error(), "no live render has recorded its flags") {
		t.Fatalf("err = %v, want a refusal naming the recorded flags", err)
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want no hold taken", f.events.calls)
	}
}

// TestTownClusterRollRefusesARealRollWithoutItsSeams: the production build wires
// neither the hold store nor the two wait reads, so a real roll refuses before
// it reads the cluster, and --dry-run is the path that runs.
func TestTownClusterRollRealRollNeedsTheHoldAndWaits(t *testing.T) {
	t.Parallel()
	d := clusterRollDeps{
		apply: &clusterRollApplier{
			kubectl: func(context.Context, string, []string, string, ...string) (string, int, error) {
				t.Error("read the cluster before refusing an unwired hold")
				return "", 0, nil
			},
			render:      clusterRenderDepsAt(t, t.TempDir(), io.Discard),
			rolloutWait: time.Minute,
		},
		signals: liveRollSignals,
		out:     io.Discard,
	}
	err := runTownClusterRoll(d)
	if err == nil || !strings.Contains(err.Error(), "landing hold store") {
		t.Fatalf("err = %v, want a refusal naming the hold store", err)
	}
}

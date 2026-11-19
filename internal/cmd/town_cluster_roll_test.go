package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	k8s "github.com/steveyegge/gastown/deploy/k8s"
	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
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

// isRevisionRead reports whether a call reads a Deployment's recorded revision,
// the read the rollback lines make before the first apply.
func isRevisionRead(args []string) bool {
	return strings.Contains(strings.Join(args, " "), "deployment\\.kubernetes\\.io/revision")
}

// clusterRollAnswering answers the cluster calls a roll makes beyond the diff's
// reads: an apply succeeds, a rollout status succeeds, a ready read answers True
// and a changed Deployment records revision 7. The Deployment drift reads go to
// drift, which the drift tests' own answering supplies.
func clusterRollAnswering(t *testing.T, drift update.RunFunc) update.RunFunc {
	t.Helper()
	return clusterRollAnsweringRevisions(t, drift, func(string) (string, int, error) { return "7", 0, nil })
}

// clusterRollAnsweringRevisions is clusterRollAnswering with the answer a
// Deployment's revision read gives, so a test drives a revision it cannot read.
func clusterRollAnsweringRevisions(t *testing.T, drift update.RunFunc, revision func(name string) (string, int, error)) update.RunFunc {
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
			if isRevisionRead(args) {
				return revision(args[2])
			}
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

// noDifferencesLine is the line the roll prints when the compare finds nothing.
// The compare covers the Deployments only, so the line must say that and nothing
// wider.
func noDifferencesLine(t *testing.T, out *bytes.Buffer) string {
	t.Helper()
	for _, line := range driftLines(out) {
		if strings.HasPrefix(line, "no differences:") {
			return line
		}
	}
	t.Fatalf("the roll printed no no-differences line; printed:\n%s", out.String())
	return ""
}

// driftServiceChanged makes a live Service differ from the render, the change
// the roll's Deployment-only compare does not see.
func driftServiceChanged(t *testing.T, live map[string]any) {
	t.Helper()
	if live["kind"] != "Service" {
		return
	}
	driftObjectAt(live, "metadata", "annotations")["example.test/hand-patched"] = "true"
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
// the render applies the workload kinds in the roll's order and waits for each
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
		"deployment/bd-gateway",
		"deployment/daemon",
		"deployment/dashboard",
		"deployment/trackerd",
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

// TestTownClusterRollEmptyDiffTakesNoHold: a cluster whose Deployments already
// match the render is never restarted, so no hold is taken and nothing is
// applied. The line says the Deployments match, never the namespace or the
// manifests, since only the Deployments were compared.
func TestTownClusterRollEmptyDiffTakesNoHold(t *testing.T) {
	t.Parallel()
	d, _, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	f := newClusterRollFixture(t, d.render, clusterRollAnswering(t, driftAnswering(t, m, nil)))

	if err := runTownClusterRoll(f.deps); err != nil {
		t.Fatalf("roll: %v\nprinted:\n%s", err, f.out.String())
	}
	line := noDifferencesLine(t, f.out)
	if !strings.Contains(line, "the live Deployments match the rendered Deployments") {
		t.Errorf("the empty-diff line = %q, want it to name the Deployments", line)
	}
	for _, banned := range []string{"namespace", "manifests"} {
		if strings.Contains(line, banned) {
			t.Errorf("the empty-diff line = %q, want no %q, since only the Deployments were compared", line, banned)
		}
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

// TestTownClusterRollIgnoresAServiceOnlyDifference: the compare covers only the
// Deployments, so a live Service that differs from the render is not seen. The
// roll reports the Deployments matching, takes no hold and applies nothing,
// which pins the stated limit.
func TestTownClusterRollIgnoresAServiceOnlyDifference(t *testing.T) {
	t.Parallel()
	d, _, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	f := newClusterRollFixture(t, d.render, clusterRollAnswering(t, driftAnswering(t, m, driftServiceChanged)))

	if err := runTownClusterRoll(f.deps); err != nil {
		t.Fatalf("roll: %v\nprinted:\n%s", err, f.out.String())
	}
	if line := noDifferencesLine(t, f.out); !strings.Contains(line, "the live Deployments match the rendered Deployments") {
		t.Errorf("the empty-diff line = %q, want it to name the Deployments", line)
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want no hold taken", f.events.calls)
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
		{
			name:    "a static document that names nothing",
			object:  k8s.StaticObject{},
			wantErr: "names no kind and no name",
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
	if !strings.Contains(f.out.String(), "the live Deployments differ from the rendered Deployments:") {
		t.Errorf("the dry run did not name the Deployments it compared; printed:\n%s", f.out.String())
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

// TestTownClusterRollDryRunSaysTheDeploymentsMatch: a dry run whose Deployments
// match prints the Deployments-only line, never a whole-namespace match, and
// takes no hold.
func TestTownClusterRollDryRunSaysTheDeploymentsMatch(t *testing.T) {
	t.Parallel()
	d, _, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	f := newClusterRollFixture(t, d.render, clusterRollAnswering(t, driftAnswering(t, m, nil)))
	f.deps.dryRun = true

	if err := runTownClusterRoll(f.deps); err != nil {
		t.Fatalf("dry run: %v\nprinted:\n%s", err, f.out.String())
	}
	line := noDifferencesLine(t, f.out)
	if !strings.Contains(line, "the live Deployments match the rendered Deployments") {
		t.Errorf("the dry run's empty-diff line = %q, want it to name the Deployments", line)
	}
	for _, banned := range []string{"namespace", "manifests"} {
		if strings.Contains(line, banned) {
			t.Errorf("the dry run's empty-diff line = %q, want no %q", line, banned)
		}
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want no hold taken", f.events.calls)
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
				case len(args) > 1 && args[0] == "get" && args[1] == "deployment" && isRevisionRead(args):
					// The rollback lines read this before the first apply, so the
					// case below drives the wait, not the revision.
					return "7", 0, nil
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

// driftGatewayAndDashboardChanged moves the live bd gateway's readiness probe and
// the live dashboard's TZ value away from the render, so two Deployments differ
// and the roll prints two undo lines.
func driftGatewayAndDashboardChanged(t *testing.T, live map[string]any) {
	t.Helper()
	driftGatewayProbeMoved(t, live)
	if live["kind"] != "Deployment" || live["metadata"].(map[string]any)["name"] != k8sdaemon.DashboardName {
		return
	}
	driftEnvOf(t, driftContainer(t, live, k8sdaemon.DashboardName), "TZ")["value"] = "UTC"
}

// clusterRollTwoChangedDeployments is the four-Deployment fixture whose live
// gateway and dashboard both differ from the render, with the gateway's rollout
// finishing but its Available condition False.
func clusterRollTwoChangedDeployments(t *testing.T) *clusterRollFixture {
	t.Helper()
	d, _, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	revisions := map[string]string{k8sdaemon.BdGatewayName: "7", k8sdaemon.DashboardName: "4"}
	answer := clusterRollAnsweringRevisions(t, driftAnswering(t, m, driftGatewayAndDashboardChanged), func(name string) (string, int, error) {
		return revisions[name], 0, nil
	})
	return newClusterRollFixture(t, d.render, func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		if len(args) > 2 && args[0] == "get" && args[1] == "deployment" && !isRevisionRead(args) && args[2] == k8sdaemon.BdGatewayName {
			return "False", 0, nil
		}
		return answer(ctx, dir, env, name, args...)
	})
}

// kubectlRollSequence is the cluster calls a roll made from its first revision
// read on, which is after the compare: each apply named by the object it wrote
// on stdin, and each wait and revision read by what it read.
func kubectlRollSequence(t *testing.T, k *recordingKubectl) []string {
	t.Helper()
	var sequence []string
	started := false
	for _, call := range k.calls {
		switch {
		case call.args[0] == "get" && isRevisionRead(call.args):
			started = true
			sequence = append(sequence, "revision "+call.args[2])
		case !started:
		case call.args[0] == "apply":
			var object struct {
				Kind     string
				Metadata struct{ Name string }
			}
			if err := yaml.Unmarshal([]byte(call.stdin), &object); err != nil {
				t.Fatalf("decoding the applied object: %v", err)
			}
			sequence = append(sequence, "apply "+strings.ToLower(object.Kind)+"/"+object.Metadata.Name)
		case call.args[0] == "rollout":
			sequence = append(sequence, "rollout "+call.args[2])
		case call.args[0] == "get":
			sequence = append(sequence, "ready deployment/"+call.args[2])
		default:
			t.Fatalf("unexpected kubectl call %v", call.args)
		}
	}
	return sequence
}

// TestTownClusterRollWaitsForEachDeploymentBeforeTheNextApply: the bd gateway is
// applied first of the Deployments and its rollout and Available condition are
// waited for before the daemon is applied, then the same for each Deployment in
// turn, so a gateway that does not come back is never followed by the daemon it
// fronts being replaced.
func TestTownClusterRollWaitsForEachDeploymentBeforeTheNextApply(t *testing.T) {
	t.Parallel()
	f := clusterRollFourDeployments(t)

	if err := runTownClusterRoll(f.deps); err != nil {
		t.Fatalf("roll: %v\nprinted:\n%s", err, f.out.String())
	}

	want := []string{
		"revision bd-gateway",
		"apply persistentvolumeclaim/town-state",
		"apply persistentvolumeclaim/landing-work",
		"apply persistentvolumeclaim/dolt-backup",
		"apply persistentvolumeclaim/bd-gateway-workspace",
		"apply persistentvolumeclaim/bd-gateway-log",
		"apply serviceaccount/daemon",
		"apply service/forgejo",
		"apply service/dashboard",
		"apply service/trackerd",
		"apply service/bd-gateway",
		"apply service/bd-gateway-push",
		"apply deployment/bd-gateway",
		"rollout deployment/bd-gateway",
		"ready deployment/bd-gateway",
		"apply deployment/daemon",
		"rollout deployment/daemon",
		"ready deployment/daemon",
		"apply deployment/dashboard",
		"rollout deployment/dashboard",
		"ready deployment/dashboard",
		"apply deployment/trackerd",
		"rollout deployment/trackerd",
		"ready deployment/trackerd",
	}
	if got := kubectlRollSequence(t, f.kubectl); !slices.Equal(got, want) {
		t.Errorf("cluster calls = %v, want %v", got, want)
	}
}

// TestTownClusterRollGatewayNotReadyStopsAndPrintsTheRollbackLines: a gateway
// whose rollout finishes but that never becomes Available stops the roll with the
// daemon, the dashboard and trackerd unapplied, gives the landing hold back,
// exits non-zero and leaves the undo line of each changing Deployment, with the
// revision the cluster recorded.
func TestTownClusterRollGatewayNotReadyStopsAndPrintsTheRollbackLines(t *testing.T) {
	t.Parallel()
	f := clusterRollTwoChangedDeployments(t)

	err := runTownClusterRoll(f.deps)
	if err == nil || !strings.Contains(err.Error(), "deployment/bd-gateway is not ready") {
		t.Fatalf("err = %v, want the gateway's missing readiness", err)
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
		"deployment/bd-gateway",
	}
	if got := appliedObjects(f.out); !slices.Equal(got, wantApplied) {
		t.Errorf("applied = %v, want the gateway and nothing after it", got)
	}
	for _, call := range f.kubectl.calls {
		if call.args[0] == "rollout" && call.args[2] != "deployment/bd-gateway" {
			t.Errorf("waited for %s after the gateway did not come back", call.args[2])
		}
		if slices.Contains(call.args, "undo") {
			t.Errorf("the roll ran %v; it prints the undo command rather than rolling back", call.args)
		}
	}
	if f.store.present {
		t.Error("the landing hold is still in place after the gateway did not come back")
	}
	if !slices.Contains(f.events.calls, "hold:remove") {
		t.Errorf("calls = %v, want the hold released", f.events.calls)
	}
	lines := driftLines(f.out)
	wantLines := []string{
		"kubectl rollout undo deployment/bd-gateway -n gt --to-revision=7",
		"kubectl rollout undo deployment/dashboard -n gt --to-revision=4",
	}
	for _, line := range wantLines {
		if !slices.Contains(lines, line) {
			t.Errorf("the roll never printed %q; printed:\n%s", line, f.out.String())
		}
	}
}

// TestTownClusterRollStopsBeforeAnyApplyWhenARevisionIsUnreadable: every way a
// changed Deployment's recorded revision can be missing stops the roll before the
// first apply, so a printed undo command always names a revision that exists, and
// the landing hold is given back.
func TestTownClusterRollStopsBeforeAnyApplyWhenARevisionIsUnreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		revision func(string) (string, int, error)
		wantErr  string
	}{
		{
			name:     "a changed Deployment records no revision",
			revision: func(string) (string, int, error) { return "", 0, nil },
			wantErr:  "deployment/bd-gateway records no revision",
		},
		{
			name:     "the revision read cannot start",
			revision: func(string) (string, int, error) { return "", 0, errors.New("exec: kubectl not found") },
			wantErr:  "exec: kubectl not found",
		},
		{
			name:     "the revision read exits non-zero",
			revision: func(string) (string, int, error) { return "", 1, nil },
			wantErr:  "kubectl exited 1",
		},
		{
			name:     "the revision is not a number",
			revision: func(string) (string, int, error) { return "seven", 0, nil },
			wantErr:  "not a positive number",
		},
		{
			name:     "the revision is zero",
			revision: func(string) (string, int, error) { return "0", 0, nil },
			wantErr:  "not a positive number",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, town := driftFourDeploymentSeams(t)
			m := driftTestRendered(t, d, driftTestFlags(t, town))
			f := newClusterRollFixture(t, d.render, clusterRollAnsweringRevisions(t, driftAnswering(t, m, driftGatewayAndDashboardChanged), tc.revision))

			err := runTownClusterRoll(f.deps)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
			}
			if got := appliedObjects(f.out); len(got) != 0 {
				t.Errorf("applied = %v, want nothing before a revision that could be read", got)
			}
			if f.store.present {
				t.Error("the landing hold is still in place after a revision could not be read")
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

// clusterRollFlagsCmd is the cluster roll command with every flag defined except
// missing, so a flag the roll cannot read is reachable without a bad command.
func clusterRollFlagsCmd(missing string) *cobra.Command {
	cmd := &cobra.Command{}
	if missing != "landing-wait" {
		cmd.Flags().Duration("landing-wait", 10*time.Minute, "")
	}
	if missing != "runs-wait" {
		cmd.Flags().Duration("runs-wait", 20*time.Minute, "")
	}
	if missing != "rollout-wait" {
		cmd.Flags().Duration("rollout-wait", 10*time.Minute, "")
	}
	if missing != "dry-run" {
		cmd.Flags().Bool("dry-run", false, "")
	}
	return cmd
}

// TestTownClusterRollRefusesAFlagItCannotRead: a flag the command does not carry
// stops the roll before the town is looked up.
func TestTownClusterRollRefusesAFlagItCannotRead(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"landing-wait", "runs-wait", "rollout-wait", "dry-run"} {
		findTown := func() (string, error) {
			t.Errorf("the town was looked up although --%s could not be read", name)
			return "", nil
		}

		err := runTownClusterRollIn(clusterRollFlagsCmd(name), findTown)

		if err == nil || !strings.Contains(err.Error(), "reading --"+name) {
			t.Errorf("--%s missing: err = %v, want it to name reading --%s", name, err, name)
		}
	}
}

// TestTownClusterRollRefusesWhenTheTownIsNotFound: the roll names the lookup it
// could not make rather than running outside a town.
func TestTownClusterRollRefusesWhenTheTownIsNotFound(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	cmd := clusterRollFlagsCmd("")
	cmd.SetOut(out)
	findTown := func() (string, error) { return "", errors.New("no town above the working directory") }

	err := runTownClusterRollIn(cmd, findTown)

	if err == nil || !strings.Contains(err.Error(), "finding the town root") {
		t.Fatalf("err = %v, want it to name finding the town root", err)
	}
	if out.Len() != 0 {
		t.Errorf("the roll printed %q before refusing", out.String())
	}
}

// TestTownClusterRollRefusesWithoutItsTwoSeams: a roll with nowhere to print and
// a real roll with no signal source each refuse before the hold.
func TestTownClusterRollRefusesWithoutItsTwoSeams(t *testing.T) {
	t.Parallel()
	t.Run("nowhere to print", func(t *testing.T) {
		t.Parallel()
		err := runTownClusterRoll(clusterRollDeps{})
		if err == nil || !strings.Contains(err.Error(), "nowhere to print") {
			t.Fatalf("err = %v, want a refusal with nowhere to print", err)
		}
	})
	t.Run("no signal source", func(t *testing.T) {
		t.Parallel()
		f := clusterRollFourDeployments(t)
		f.deps.signals = nil

		err := runTownClusterRoll(f.deps)
		if err == nil || !strings.Contains(err.Error(), "no signal source") {
			t.Fatalf("err = %v, want a refusal naming the signal source", err)
		}
		if len(f.events.calls) != 0 {
			t.Errorf("calls = %v, want no hold taken", f.events.calls)
		}
	})
}

// TestTownClusterRollRefusesAnUnreadableRender: each input the render needs, and
// its build, refuses the roll before the hold and before any cluster read.
func TestTownClusterRollRefusesAnUnreadableRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		render  func(t *testing.T) clusterRenderDeps
		wantErr string
	}{
		{
			name: "not in a town",
			render: func(t *testing.T) clusterRenderDeps {
				render := clusterRenderDepsAt(t, t.TempDir(), io.Discard)
				render.findTown = func() (string, error) { return "", errors.New("no town above the working directory") }
				return render
			},
			wantErr: "not in a Gas Town workspace",
		},
		{
			name: "the recorded flags are refused",
			render: func(t *testing.T) clusterRenderDeps {
				town := t.TempDir()
				path := renderFlagsPathIn(town)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("making the flags directory: %v", err)
				}
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatalf("writing the broken flags file: %v", err)
				}
				return clusterRenderDepsAt(t, town, io.Discard)
			},
			wantErr: "are refused",
		},
		{
			name: "no image pair",
			render: func(t *testing.T) clusterRenderDeps {
				d, _, town := driftTestSeams(t, nil)
				dir := driftTestFlags(t, town).dir
				if err := os.Remove(imagevalues.PathIn(dir)); err != nil {
					t.Fatalf("removing the values file: %v", err)
				}
				return d.render
			},
			wantErr: "holds no image pair",
		},
		{
			name: "no secrets manifest",
			render: func(t *testing.T) clusterRenderDeps {
				d, _, town := driftTestSeams(t, nil)
				dir := driftTestFlags(t, town).dir
				if err := os.Remove(filepath.Join(dir, secretsManifestRelPath)); err != nil {
					t.Fatalf("removing the secrets manifest: %v", err)
				}
				return d.render
			},
			wantErr: "reading the secrets manifest",
		},
		{
			name: "the build refuses",
			render: func(t *testing.T) clusterRenderDeps {
				d, _, _ := driftTestSeams(t, nil)
				render := d.render
				render.build = func(k8sdaemon.Values) (k8sdaemon.Manifests, error) {
					return k8sdaemon.Manifests{}, errors.New("the values cannot be rendered")
				}
				return render
			},
			wantErr: "cannot render the daemon namespace's manifests",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newClusterRollFixture(t, tc.render(t), func(context.Context, string, []string, string, ...string) (string, int, error) {
				t.Error("read the cluster before refusing an unreadable render")
				return "", 0, nil
			})

			err := runTownClusterRoll(f.deps)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
			}
			if len(f.events.calls) != 0 {
				t.Errorf("calls = %v, want no hold taken", f.events.calls)
			}
		})
	}
}

// TestTownClusterRollRendersOnce: one real roll builds the render once and looks
// the town up once, so the diff, the apply and the ready wait cannot read
// different renders.
func TestTownClusterRollRendersOnce(t *testing.T) {
	t.Parallel()
	d, _, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	render := d.render
	build := render.build
	builds := 0
	render.build = func(v k8sdaemon.Values) (k8sdaemon.Manifests, error) {
		builds++
		return build(v)
	}
	findTown := render.findTown
	lookups := 0
	render.findTown = func() (string, error) {
		lookups++
		return findTown()
	}
	f := newClusterRollFixture(t, render, clusterRollAnswering(t, driftAnswering(t, m, driftGatewayProbeMoved)))

	if err := runTownClusterRoll(f.deps); err != nil {
		t.Fatalf("roll: %v\nprinted:\n%s", err, f.out.String())
	}
	if builds != 1 {
		t.Errorf("the render was built %d times, want once", builds)
	}
	if lookups != 1 {
		t.Errorf("the town was looked up %d times, want once", lookups)
	}
}

// TestTownClusterRollApplyRefusesAnObjectItCannotEncode: a manifest the encoder
// cannot write stops the apply, and the hold is released.
func TestTownClusterRollApplyRefusesAnObjectItCannotEncode(t *testing.T) {
	t.Parallel()
	d, _, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	render := d.render
	encode := render.encode
	render.encode = func(w io.Writer, object any) error {
		if _, ok := object.(k8sdaemon.Deployment); ok {
			return encode(w, object)
		}
		return errors.New("the object cannot be encoded")
	}
	f := newClusterRollFixture(t, render, clusterRollAnswering(t, driftAnswering(t, m, driftGatewayProbeMoved)))

	err := runTownClusterRoll(f.deps)
	if err == nil || !strings.Contains(err.Error(), "building the persistentvolumeclaim town-state") {
		t.Fatalf("err = %v, want the unencodable claim", err)
	}
	if f.store.present {
		t.Error("the landing hold is still in place after an apply that could not encode")
	}
}

// TestTownClusterRollApplyStartFailureReleasesTheHold: an apply kubectl cannot
// start ends the roll and gives the hold back, as a refused apply does.
func TestTownClusterRollApplyStartFailureReleasesTheHold(t *testing.T) {
	t.Parallel()
	d, _, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	answer := clusterRollAnswering(t, driftAnswering(t, m, driftGatewayProbeMoved))
	f := newClusterRollFixture(t, d.render, func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		if len(args) > 0 && args[0] == "apply" {
			return "", 0, errors.New("exec: kubectl not found")
		}
		return answer(ctx, dir, env, name, args...)
	})

	err := runTownClusterRoll(f.deps)
	if err == nil || !strings.Contains(err.Error(), "applying the persistentvolumeclaim town-state") {
		t.Fatalf("err = %v, want the apply kubectl could not start", err)
	}
	if f.store.present {
		t.Error("the landing hold is still in place after an apply kubectl could not start")
	}
}

// TestTownClusterRollRealRollNeedsTheHoldAndWaits: the production build wires
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

// TestClusterRollDepsUseTheTownTheCommandFound: the production deps hand the
// render the root the command looked up, so the roll does not search for the
// town a second time.
func TestClusterRollDepsUseTheTownTheCommandFound(t *testing.T) {
	t.Parallel()
	d := clusterRollDepsFor("/town", io.Discard, time.Minute, time.Minute, time.Minute, false)

	got, err := d.apply.render.findTown()
	if err != nil || got != "/town" {
		t.Errorf("the render's town = %q, %v, want the /town the command found", got, err)
	}
}

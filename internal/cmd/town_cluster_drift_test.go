package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/update"
)

// driftTestSeams is a drift run's host: a town holding the recorded flags, a repo
// holding the values file and the secrets manifest, and the render's own steps
// over them. adjust edits the flags before they are recorded. The caller wires
// kubectl, the only read the run makes of a cluster.
func driftTestSeams(t *testing.T, adjust func(*clusterRenderFlags)) (clusterDriftDeps, *bytes.Buffer, string) {
	t.Helper()
	town := clusterRenderTestTown(t)
	flags := clusterRenderFixture(t)
	if adjust != nil {
		adjust(&flags)
	}
	if err := saveRenderFlags(renderFlagsPathIn(town), flags); err != nil {
		t.Fatalf("recording the render flags: %v", err)
	}
	out := &bytes.Buffer{}
	return clusterDriftDeps{render: clusterRenderDepsAt(t, town, io.Discard), runner: realRunnerDriftTargets, out: out}, out, town
}

// driftTestFlags is the flag set the run reads back from the town.
func driftTestFlags(t *testing.T, town string) clusterRenderFlags {
	t.Helper()
	flags, ok, err := loadRenderFlags(renderFlagsPathIn(town))
	if err != nil || !ok {
		t.Fatalf("reading the recorded flags: ok=%v, err=%v", ok, err)
	}
	return flags
}

// driftTestRendered is the manifests the run's own flags render, which each live
// answer starts from.
func driftTestRendered(t *testing.T, d clusterDriftDeps, flags clusterRenderFlags) k8sdaemon.Manifests {
	t.Helper()
	v, _, _, err := clusterRenderValues(d.render, flags, d.render.findTown)
	if err != nil {
		t.Fatalf("building the Values: %v", err)
	}
	m, err := d.render.build(v)
	if err != nil {
		t.Fatalf("rendering the manifests: %v", err)
	}
	return m
}

// driftAnswering answers each kubectl read with the live form of the object the
// run rendered, then applies mutate to the Deployments the flags render. The
// runner's objects come from the control values, as the run's own render does,
// and carry no mutation, since a case's mutation names a flag-rendered object.
func driftAnswering(t *testing.T, m k8sdaemon.Manifests, mutate func(*testing.T, map[string]any)) update.RunFunc {
	t.Helper()
	objects := map[string]any{}
	for resource, deployment := range map[string]k8sdaemon.Deployment{
		"deployment/" + k8sdaemon.Name:          m.Deployment,
		"deployment/" + k8sdaemon.DashboardName: m.DashboardDeployment,
		"deployment/" + k8sdaemon.TrackerName:   m.TrackerDeployment,
		"deployment/" + k8sdaemon.BdGatewayName: m.BdGatewayDeployment,
	} {
		if deployment.Kind != "" {
			objects[resource] = deployment
		}
	}
	runnerTargets, err := realRunnerDriftTargets()
	if err != nil {
		t.Fatalf("rendering the gate runner: %v", err)
	}
	runner := map[string]any{}
	for _, target := range runnerTargets {
		runner[target.resource] = target.object
	}
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
		if name != "kubectl" {
			t.Fatalf("ran %q, want kubectl", name)
		}
		if len(args) < 2 || args[0] != "get" || args[len(args)-1] != "json" {
			t.Fatalf("ran kubectl %q, want a get with -o json", args)
		}
		if object, ok := runner[args[1]]; ok {
			return driftLiveAnswer(t, object, nil), 0, nil
		}
		object, ok := objects[args[1]]
		if !ok {
			t.Fatalf("kubectl read %q, which the run did not render", args[1])
		}
		return driftLiveAnswer(t, object, mutate), 0, nil
	}
}

// driftNoRead is the kubectl runner of a run that must refuse before it reads the
// cluster: a read is the test's failure.
func driftNoRead(t *testing.T) update.RunFunc {
	return func(context.Context, string, []string, string, ...string) (string, int, error) {
		t.Fatal("read the cluster before refusing an input it could not read")
		return "", 0, nil
	}
}

// driftLiveAnswer returns the rendered object as kubectl reports the live one: a
// JSON round trip, so its numbers are float64 as encoding/json makes them, plus
// the fields and defaults the API server adds, then mutate.
func driftLiveAnswer(t *testing.T, rendered any, mutate func(*testing.T, map[string]any)) string {
	t.Helper()
	live := driftDecodedObject(t, rendered)
	driftServerFields(live)
	if mutate != nil {
		mutate(t, live)
	}
	encoded, err := json.Marshal(live)
	if err != nil {
		t.Fatalf("encoding the live answer: %v", err)
	}
	return string(encoded)
}

// driftDecodedObject decodes a rendered object to the field tree
// encoding/json makes of the same object.
func driftDecodedObject(t *testing.T, rendered any) map[string]any {
	t.Helper()
	var body bytes.Buffer
	if err := k8sdaemon.YAML(&body, rendered); err != nil {
		t.Fatalf("encoding the rendered object: %v", err)
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(body.Bytes(), &decoded); err != nil {
		t.Fatalf("decoding the rendered object: %v", err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("encoding the rendered Deployment as JSON: %v", err)
	}
	var live map[string]any
	if err := json.Unmarshal(encoded, &live); err != nil {
		t.Fatalf("decoding the rendered Deployment as JSON: %v", err)
	}
	return live
}

// driftServerFields adds what the API server fills in after the render is
// applied: the metadata it stamps, the defaults it sets, and a status. A
// ConfigMap gets only the metadata, since the server sets no pod fields on it.
func driftServerFields(live map[string]any) {
	metadata := driftObjectAt(live, "metadata")
	metadata["resourceVersion"] = "12345"
	metadata["uid"] = "5d0e6a2c-0000-4000-8000-000000000000"
	metadata["creationTimestamp"] = "2026-11-15T10:00:00Z"
	if live["kind"] != "Deployment" {
		return
	}
	metadata["generation"] = 4
	driftObjectAt(metadata, "annotations")["deployment.kubernetes.io/revision"] = "4"

	templateMeta := driftTemplateMeta(live)
	templateMeta["creationTimestamp"] = nil
	driftObjectAt(templateMeta, "annotations")[restartedAtAnnotation] = "2026-11-15T10:30:00Z"

	podSpec := driftPodSpec(live)
	podSpec["dnsPolicy"] = "ClusterFirst"
	podSpec["restartPolicy"] = "Always"
	// The server defaults the grace period only when the render names none, as
	// the daemon's render does; the runner's names its own.
	if _, named := podSpec["terminationGracePeriodSeconds"]; !named {
		podSpec["terminationGracePeriodSeconds"] = 30
	}

	spec := driftObjectAt(live, "spec")
	spec["progressDeadlineSeconds"] = 600
	spec["revisionHistoryLimit"] = 10

	for _, entry := range driftPodContainers(live) {
		container := entry.(map[string]any)
		container["terminationMessagePath"] = "/dev/termination-log"
		container["imagePullPolicy"] = "IfNotPresent"
	}
	live["status"] = map[string]any{"readyReplicas": float64(1)}
}

// driftObjectAt walks decoded objects by key, making each one it passes through,
// so the fixture can add a field the render left out.
func driftObjectAt(root map[string]any, keys ...string) map[string]any {
	current := root
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[key] = next
		}
		current = next
	}
	return current
}

// driftExit is the process status a finished run leaves: 0 for no error, else the
// code the command maps its error to.
func driftExit(err error) int {
	if err == nil {
		return 0
	}
	return exitCodeForError(err)
}

// driftLines splits what a run printed into its lines.
func driftLines(out *bytes.Buffer) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// driftNameBdImage rewrites the repo's values file to name a bd image, so a render
// of the flags builds the bd gateway.
func driftNameBdImage(t *testing.T, town string) {
	t.Helper()
	dir := driftTestFlags(t, town).dir
	if err := imagevalues.Write(imagevalues.PathIn(dir), imagevalues.Values{
		Daemon:  clusterRenderDaemonDigest,
		Polecat: clusterRenderDaemonDigest,
		Commit:  strings.Repeat("a", 40),
		Bd:      "registry.example.test/gastown-bd@sha256:" + strings.Repeat("3", 64),
	}); err != nil {
		t.Fatalf("writing the values file with a bd image: %v", err)
	}
}

// driftFourDeploymentSeams is a drift host whose flags render the dashboard,
// trackerd and the bd gateway beside the daemon, so a run has four Deployments to
// read.
func driftFourDeploymentSeams(t *testing.T) (clusterDriftDeps, *bytes.Buffer, string) {
	t.Helper()
	d, out, town := driftTestSeams(t, func(f *clusterRenderFlags) {
		f.dashboard = true
		f.tracker = true
		f.trackerDSNSecret = "gt-trackerd-dsn"
		f.proxyImage = clusterRenderDaemonDigest
		f.hostGateway = "192.168.64.1"
		f.bdGatewayTokenSecret = "bd-gateway-tokens"
		// The bd image renders the push URL, which needs the operator's
		// attestation that the Service admission policy is applied.
		f.bdGatewayServicePolicyApplied = true
	})
	driftNameBdImage(t, town)
	return d, out, town
}

// driftRecordReads wraps answer so each Deployment the run asks kubectl for is
// kept, in the order it asks.
func driftRecordReads(answer update.RunFunc, reads *[]string) update.RunFunc {
	return func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		if len(args) > 1 {
			*reads = append(*reads, args[1])
		}
		return answer(ctx, dir, env, name, args...)
	}
}

// TestTownClusterDriftComparesTrackerdAndTheBdGateway holds a render of the
// dashboard, trackerd and the bd gateway to four live reads by name, and a gateway
// difference to a line naming deployment/bd-gateway and its JSON path.
func TestTownClusterDriftComparesTrackerdAndTheBdGateway(t *testing.T) {
	t.Parallel()
	d, out, town := driftFourDeploymentSeams(t)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	var reads []string
	d.kubectl = driftRecordReads(driftAnswering(t, m, func(_ *testing.T, live map[string]any) {
		if live["metadata"].(map[string]any)["name"] != k8sdaemon.BdGatewayName {
			return
		}
		// The cluster has not rolled yet: it still runs Recreate, with no rollingUpdate.
		strategy := driftObjectAt(live, "spec", "strategy")
		strategy["type"] = "Recreate"
		delete(strategy, "rollingUpdate")
	}), &reads)
	err := runClusterDrift(context.Background(), d)
	if got := driftExit(err); got != 1 {
		t.Fatalf("exit = %d, want 1 (err: %v); printed:\n%s", got, err, out.String())
	}
	wantReads := []string{
		"deployment/" + k8sdaemon.Name,
		"deployment/" + k8sdaemon.DashboardName,
		"deployment/" + k8sdaemon.TrackerName,
		"deployment/" + k8sdaemon.BdGatewayName,
		"deployment/" + forgejorunner.Name,
		"configmap/" + forgejorunner.Name + "-config",
	}
	if !slices.Equal(reads, wantReads) {
		t.Errorf("read %q, want %q", reads, wantReads)
	}
	// The render rolls the gateway; the cluster still holds the Recreate it ran before.
	gateway := "deployment/" + k8sdaemon.BdGatewayName + ": "
	wantLines := []string{
		gateway + driftWantAbsent(t, "spec.strategy.rollingUpdate", map[string]any{"maxUnavailable": 0, "maxSurge": 1}),
		gateway + driftWantDifference(t, "spec.strategy.type", "RollingUpdate", "Recreate"),
	}
	if got := driftLines(out); !slices.Equal(got, wantLines) {
		t.Errorf("printed\n%q\nwant\n%q", got, wantLines)
	}
}

// TestTownClusterDriftRefusesATrackerdOrGatewayTheClusterDoesNotHold holds a
// Deployment the render builds and the cluster does not hold to the unreadable
// exit status, never 0 or 1.
func TestTownClusterDriftRefusesATrackerdOrGatewayTheClusterDoesNotHold(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		resource string
	}{
		{name: "the bd gateway is absent", resource: "deployment/" + k8sdaemon.BdGatewayName},
		{name: "trackerd is absent", resource: "deployment/" + k8sdaemon.TrackerName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, town := driftFourDeploymentSeams(t)
			answer := driftAnswering(t, driftTestRendered(t, d, driftTestFlags(t, town)), nil)
			d.kubectl = func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
				if len(args) > 1 && args[1] == tc.resource {
					// kubectl's status for a Deployment the cluster does not hold.
					return "", 1, nil
				}
				return answer(ctx, dir, env, name, args...)
			}
			err := runClusterDrift(context.Background(), d)
			if got := driftExit(err); got != driftExitUnreadable {
				t.Fatalf("exit = %d, want %d (err: %v); printed:\n%s", got, driftExitUnreadable, err, out.String())
			}
			if want := "the live " + tc.resource + " could not be read"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not hold %q", err, want)
			}
			if got := driftLines(out); got != nil {
				t.Errorf("printed %q beside the refusal, want nothing", got)
			}
		})
	}
}

// TestTownClusterDriftReadsOnlyTheDeploymentsTheFlagsRender holds flags that render
// no trackerd and no bd gateway to the two flag-rendered reads the run made
// before, the daemon and the dashboard, followed by the runner the control
// values always render.
func TestTownClusterDriftReadsOnlyTheDeploymentsTheFlagsRender(t *testing.T) {
	t.Parallel()
	d, out, town := driftTestSeams(t, func(f *clusterRenderFlags) { f.dashboard = true })
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	var reads []string
	d.kubectl = driftRecordReads(driftAnswering(t, m, nil), &reads)
	err := runClusterDrift(context.Background(), d)
	if got := driftExit(err); got != 0 {
		t.Fatalf("exit = %d, want 0 (err: %v); printed:\n%s", got, err, out.String())
	}
	wantReads := []string{
		"deployment/" + k8sdaemon.Name,
		"deployment/" + k8sdaemon.DashboardName,
		"deployment/" + forgejorunner.Name,
		"configmap/" + forgejorunner.Name + "-config",
	}
	if !slices.Equal(reads, wantReads) {
		t.Errorf("read %q, want %q", reads, wantReads)
	}
}

// TestTownClusterDriftReportsOnlyDifferencesThatAreNotAnImageRoll drives the
// command over a live Deployment that differs from the render in the one way a
// case names, and holds the exit status and the printed lines.
func TestTownClusterDriftReportsOnlyDifferencesThatAreNotAnImageRoll(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		adjust    func(*clusterRenderFlags)
		mutate    func(t *testing.T, live map[string]any)
		wantExit  int
		wantLines []string
	}{
		{
			name:   "an image roll and the fields the server fills in, dashboard included",
			adjust: func(f *clusterRenderFlags) { f.dashboard = true },
			mutate: func(t *testing.T, live map[string]any) {
				for _, entry := range driftContainers(live) {
					entry.(map[string]any)["image"] = driftDigest("rolled")
				}
				driftObjectAt(driftTemplateMeta(live), "annotations")[k8sdaemon.PolecatImageAnnotation] = driftDigest("polecat-rolled")
			},
			wantExit: 0,
		},
		{
			name: "a differing daemon env var",
			mutate: func(t *testing.T, live map[string]any) {
				driftEnvOf(t, driftContainer(t, live, k8sdaemon.Name), "TZ")["value"] = "UTC"
			},
			wantExit: 1,
			wantLines: []string{
				"deployment/daemon: " + driftWantDifference(t, "spec.template.spec.containers[name=daemon].env[name=TZ].value", "America/Chicago", "UTC"),
			},
		},
		{
			name:   "a differing dashboard env var",
			adjust: func(f *clusterRenderFlags) { f.dashboard = true },
			mutate: func(t *testing.T, live map[string]any) {
				if live["metadata"].(map[string]any)["name"] != k8sdaemon.DashboardName {
					return
				}
				driftEnvOf(t, driftContainer(t, live, k8sdaemon.DashboardName), "TZ")["value"] = "UTC"
			},
			wantExit: 1,
			wantLines: []string{
				"deployment/dashboard: " + driftWantDifference(t, "spec.template.spec.containers[name=dashboard].env[name=TZ].value", "America/Chicago", "UTC"),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, town := driftTestSeams(t, tc.adjust)
			d.kubectl = driftAnswering(t, driftTestRendered(t, d, driftTestFlags(t, town)), tc.mutate)
			err := runClusterDrift(context.Background(), d)
			if got := driftExit(err); got != tc.wantExit {
				t.Fatalf("exit = %d, want %d (err: %v); printed:\n%s", got, tc.wantExit, err, out.String())
			}
			if got := driftLines(out); !slices.Equal(got, tc.wantLines) {
				t.Errorf("printed\n%q\nwant\n%q", got, tc.wantLines)
			}
		})
	}
}

// TestTownClusterDriftRefusesAMissingRecord holds the command to the record being
// required: with no render to compare against there is nothing to report, and
// that is an error, never a clean run.
func TestTownClusterDriftRefusesAMissingRecord(t *testing.T) {
	t.Parallel()
	d, out, town := driftTestSeams(t, nil)
	if err := os.Remove(renderFlagsPathIn(town)); err != nil {
		t.Fatalf("removing the record: %v", err)
	}
	d.kubectl = driftNoRead(t)
	err := runClusterDrift(context.Background(), d)
	if err == nil {
		t.Fatalf("run exited 0 with no recorded flags; printed:\n%s", out.String())
	}
	if want := renderFlagsPathIn(town); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %s", err, want)
	}
	if got := driftLines(out); got != nil {
		t.Errorf("printed %q beside the refusal, want nothing", got)
	}
}

// TestTownClusterDriftRefusesAnUnreadableRecord holds the command to a record it
// cannot decode being an error, never an empty flag set the run renders from.
func TestTownClusterDriftRefusesAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	d, out, town := driftTestSeams(t, nil)
	record := renderFlagsPathIn(town)
	if err := os.WriteFile(record, []byte(`{"unrecorded_flag": true}`), 0o600); err != nil {
		t.Fatalf("writing the record: %v", err)
	}
	d.kubectl = driftNoRead(t)
	err := runClusterDrift(context.Background(), d)
	if err == nil {
		t.Fatalf("run exited 0 on a record it cannot decode; printed:\n%s", out.String())
	}
	if want := "the recorded render flags " + record + " are refused"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not hold %q", err, want)
	}
}

// TestTownClusterDriftRefusesWithoutATown holds the command to refusing before it
// reads anything when the workspace cannot be found.
func TestTownClusterDriftRefusesWithoutATown(t *testing.T) {
	t.Parallel()
	d, out, _ := driftTestSeams(t, nil)
	d.render.findTown = func() (string, error) { return "", errors.New("no workspace here") }
	d.kubectl = driftNoRead(t)
	err := runClusterDrift(context.Background(), d)
	if err == nil {
		t.Fatalf("run exited 0 outside a workspace; printed:\n%s", out.String())
	}
	if want := "not in a Gas Town workspace"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not hold %q", err, want)
	}
}

// TestTownClusterDriftRefusesInputsItCannotRead holds the command to the values
// file and the secrets manifest being read: each missing file is an error naming
// the file, never a render from defaults.
func TestTownClusterDriftRefusesInputsItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prepare func(t *testing.T, flags clusterRenderFlags)
		want    string
	}{
		{
			name: "the values file is absent",
			prepare: func(t *testing.T, flags clusterRenderFlags) {
				if err := os.Remove(imagevalues.PathIn(flags.dir)); err != nil {
					t.Fatalf("removing the values file: %v", err)
				}
			},
			want: "holds no image pair",
		},
		{
			name: "the secrets manifest is absent",
			prepare: func(t *testing.T, flags clusterRenderFlags) {
				if err := os.Remove(filepath.Join(flags.dir, secretsManifestRelPath)); err != nil {
					t.Fatalf("removing the secrets manifest: %v", err)
				}
			},
			want: "reading the secrets manifest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, town := driftTestSeams(t, nil)
			tc.prepare(t, driftTestFlags(t, town))
			d.kubectl = driftNoRead(t)
			err := runClusterDrift(context.Background(), d)
			if err == nil {
				t.Fatalf("run exited 0 with an input it cannot read; printed:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
		})
	}
}

// TestTownClusterDriftRefusesARenderItCannotRead holds the command to a failed
// render, an encoder that refuses and an encoded object that does not decode
// being errors before any cluster read.
func TestTownClusterDriftRefusesARenderItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		damage  func(*clusterDriftDeps)
		wantErr string
	}{
		{
			name: "the build refuses",
			damage: func(d *clusterDriftDeps) {
				d.render.build = func(k8sdaemon.Values) (k8sdaemon.Manifests, error) {
					return k8sdaemon.Manifests{}, errors.New("the image values name no digest")
				}
			},
			wantErr: "cannot render the Deployments to compare",
		},
		{
			name: "the encoder refuses",
			damage: func(d *clusterDriftDeps) {
				d.render.encode = func(io.Writer, any) error { return errors.New("no encoder") }
			},
			wantErr: "cannot be read back",
		},
		{
			name: "the encoded object does not decode",
			damage: func(d *clusterDriftDeps) {
				d.render.encode = func(w io.Writer, _ any) error {
					_, err := io.WriteString(w, "key: [unclosed\n")
					return err
				}
			},
			wantErr: "cannot be read back",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, _ := driftTestSeams(t, nil)
			tc.damage(&d)
			d.kubectl = driftNoRead(t)
			err := runClusterDrift(context.Background(), d)
			if err == nil {
				t.Fatalf("run exited 0 on a render it cannot read; printed:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not hold %q", err, tc.wantErr)
			}
		})
	}
}

// TestTownClusterDriftRefusesAnUnreadableLiveDeployment holds the command to a
// failed, a nonzero or an unreadable kubectl answer being the unreadable exit
// status, never a cluster in step.
func TestTownClusterDriftRefusesAnUnreadableLiveDeployment(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		answer   func(context.Context, string, []string, string, ...string) (string, int, error)
		wantExit int
		want     string
	}{
		{
			name: "kubectl could not run",
			answer: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "", -1, errors.New("exec: kubectl: not found")
			},
			wantExit: driftExitUnreadable,
			want:     "reading the live deployment/daemon",
		},
		{
			name: "kubectl exited nonzero",
			answer: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "", 1, nil
			},
			wantExit: driftExitUnreadable,
			want:     "kubectl exited 1",
		},
		{
			name: "the answer is not JSON",
			answer: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "<html>", 0, nil
			},
			wantExit: driftExitUnreadable,
			want:     "not a JSON object",
		},
		{
			name: "the answer is a list",
			answer: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "[]", 0, nil
			},
			wantExit: driftExitUnreadable,
			want:     "not a JSON object",
		},
		{
			name: "the live object has no fields",
			answer: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "{}", 0, nil
			},
			wantExit: driftExitUnreadable,
			want:     "the live Deployment has no fields",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, _ := driftTestSeams(t, nil)
			d.kubectl = tc.answer
			err := runClusterDrift(context.Background(), d)
			if err == nil {
				t.Fatalf("run exited 0 on an unreadable live Deployment; printed:\n%s", out.String())
			}
			if got := driftExit(err); got != tc.wantExit {
				t.Errorf("exit = %d, want %d (err: %v)", got, tc.wantExit, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
			if got := driftLines(out); got != nil {
				t.Errorf("printed %q beside the refusal, want nothing", got)
			}
		})
	}
}

// TestTownClusterDriftExits2ForEachInputItCannotRead holds the recorded flags,
// the values file, the render and a live Deployment to the unreadable exit
// status: 2, which says the run never compared, rather than 1, which says it
// compared and found a difference, and never 0.
func TestTownClusterDriftExits2ForEachInputItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prepare func(t *testing.T, d *clusterDriftDeps, town string)
		want    string
	}{
		{
			name: "the recorded flags are absent",
			prepare: func(t *testing.T, _ *clusterDriftDeps, town string) {
				t.Helper()
				if err := os.Remove(renderFlagsPathIn(town)); err != nil {
					t.Fatalf("removing the record: %v", err)
				}
			},
			want: "no live render has recorded its flags",
		},
		{
			name: "the values file is absent",
			prepare: func(t *testing.T, _ *clusterDriftDeps, town string) {
				t.Helper()
				if err := os.Remove(imagevalues.PathIn(driftTestFlags(t, town).dir)); err != nil {
					t.Fatalf("removing the values file: %v", err)
				}
			},
			want: "holds no image pair",
		},
		{
			// A directory where the file was: os.ReadFile refuses it with EISDIR
			// for every user, where a 0o000 mode is no obstacle to root.
			name: "the values file cannot be read",
			prepare: func(t *testing.T, _ *clusterDriftDeps, town string) {
				t.Helper()
				path := imagevalues.PathIn(driftTestFlags(t, town).dir)
				if err := os.Remove(path); err != nil {
					t.Fatalf("removing the values file: %v", err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatalf("making a directory where the values file was: %v", err)
				}
			},
			want: "reading the image values under",
		},
		{
			name: "the render cannot be built",
			prepare: func(t *testing.T, d *clusterDriftDeps, _ string) {
				t.Helper()
				d.render.build = func(k8sdaemon.Values) (k8sdaemon.Manifests, error) {
					return k8sdaemon.Manifests{}, errors.New("the image values name no digest")
				}
			},
			want: "cannot render the Deployments to compare",
		},
		{
			name: "the live Deployment cannot be read",
			prepare: func(t *testing.T, d *clusterDriftDeps, _ string) {
				t.Helper()
				d.kubectl = func(context.Context, string, []string, string, ...string) (string, int, error) {
					return "", 1, nil
				}
			},
			want: "kubectl exited 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, town := driftTestSeams(t, nil)
			d.kubectl = driftNoRead(t)
			tc.prepare(t, &d, town)
			err := runClusterDrift(context.Background(), d)
			if got := driftExit(err); got != driftExitUnreadable {
				t.Fatalf("exit = %d, want %d (err: %v); printed:\n%s", got, driftExitUnreadable, err, out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
			if got := driftLines(out); got != nil {
				t.Errorf("printed %q beside the refusal, want nothing", got)
			}
		})
	}
}

// The gate runner's live objects a cluster in step holds: the control render with
// the fields the API server fills in, the dind share at the cpu 8 of
// values.control.yaml, and config.yml at the capacity 2 the control values name.
// They are not derived from the render at test time, so a change to a template
// constant or the values makes the compare report the difference instead of the
// fixture following the render; when the change is wanted on the cluster too,
// regenerate them from the control render through driftLiveAnswer's server fields.
const (
	driftRunnerDeploymentFixture = "forgejo_runner_deployment.live.json"
	driftRunnerConfigFixture     = "forgejo_runner_config.live.json"
)

// driftRecordedObject reads a recorded live object, the JSON kubectl answered
// with, from testdata.
func driftRecordedObject(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading the recorded %s: %v", name, err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("decoding the recorded %s: %v", name, err)
	}
	return object
}

// driftRecordedAnswer answers the named resources with a recorded live object
// exactly as it was recorded, and every other read with answer.
func driftRecordedAnswer(t *testing.T, answer update.RunFunc, recorded map[string]map[string]any) update.RunFunc {
	t.Helper()
	return func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		if len(args) > 1 {
			if object, ok := recorded[args[1]]; ok {
				encoded, err := json.Marshal(object)
				if err != nil {
					t.Fatalf("encoding the recorded %s answer: %v", args[1], err)
				}
				return string(encoded), 0, nil
			}
		}
		return answer(ctx, dir, env, name, args...)
	}
}

// driftControlRunner is the control cluster's values, held to the share the case
// names so a test renders a runner that differs from the record.
func driftControlRunner(t *testing.T, dindCPU string) forgejorunner.Values {
	t.Helper()
	v, err := forgejorunner.ControlValues()
	if err != nil {
		t.Fatalf("reading the control cluster's values: %v", err)
	}
	if dindCPU != "" {
		v.DindResources.CPU = dindCPU
	}
	return v
}

// driftRunnerRendering is the command's runner seam rendering v.
func driftRunnerRendering(t *testing.T, v forgejorunner.Values) func() ([]driftTarget, error) {
	t.Helper()
	m, err := forgejorunner.Build(v)
	if err != nil {
		t.Fatalf("rendering the runner: %v", err)
	}
	targets := runnerDriftTargets(m)
	return func() ([]driftTarget, error) { return targets, nil }
}

// TestTownClusterDriftHoldsARunnerInStep holds the control render to the
// recorded live objects of a cluster in step: no difference on either object and
// exit 0. The runner's live answer is the checked-in record, never an object the
// test renders, so the case cannot pass by serving the runner render back to
// itself.
func TestTownClusterDriftHoldsARunnerInStep(t *testing.T) {
	t.Parallel()
	d, out, town := driftTestSeams(t, nil)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	d.kubectl = driftRecordedAnswer(t, driftAnswering(t, m, nil), driftRunnerRecorded(t))
	err := runClusterDrift(context.Background(), d)
	if got := driftExit(err); got != 0 {
		t.Fatalf("exit = %d, want 0 (err: %v); printed:\n%s", got, err, out.String())
	}
	if got := driftLines(out); got != nil {
		t.Errorf("printed %q for a cluster in step, want nothing", got)
	}
}

// TestTownClusterDriftReportsTheGateRunnersDindShareAndItsQuota holds the recorded
// cluster's dind share of 8 to a render of 6, which is one line for the cpu
// request and one for the limit, and one for config.yml, whose quota is that same
// share.
func TestTownClusterDriftReportsTheGateRunnersDindShareAndItsQuota(t *testing.T) {
	t.Parallel()
	recorded := driftRunnerRecorded(t)
	six := driftControlRunner(t, "6")
	sixManifests, err := forgejorunner.Build(six)
	if err != nil {
		t.Fatalf("rendering the runner from a dind share of 6: %v", err)
	}
	recordedConfig := recorded["configmap/"+forgejorunner.Name+"-config"]["data"].(map[string]any)["config.yml"].(string)
	deploymentLine := "deployment/" + forgejorunner.Name + ": "
	d, out, town := driftTestSeams(t, nil)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	d.runner = driftRunnerRendering(t, six)
	d.kubectl = driftRecordedAnswer(t, driftAnswering(t, m, nil), recorded)
	err = runClusterDrift(context.Background(), d)
	if got := driftExit(err); got != 1 {
		t.Fatalf("exit = %d, want 1 (err: %v); printed:\n%s", got, err, out.String())
	}
	wantLines := []string{
		deploymentLine + driftWantDifference(t, "spec.template.spec.initContainers[name=dind].resources.limits.cpu", "6", "8"),
		deploymentLine + driftWantDifference(t, "spec.template.spec.initContainers[name=dind].resources.requests.cpu", "6", "8"),
		"configmap/" + forgejorunner.Name + "-config: " + driftWantDifference(t, "data.config.yml", sixManifests.ConfigMap.Data["config.yml"], recordedConfig),
	}
	if got := driftLines(out); !slices.Equal(got, wantLines) {
		t.Errorf("printed\n%q\nwant\n%q", got, wantLines)
	}
}

// driftRunnerRecorded is the recorded live answer for the runner's Deployment and
// ConfigMap, the two objects a compare reads.
func driftRunnerRecorded(t *testing.T) map[string]map[string]any {
	t.Helper()
	return map[string]map[string]any{
		"deployment/" + forgejorunner.Name:            driftRecordedObject(t, driftRunnerDeploymentFixture),
		"configmap/" + forgejorunner.Name + "-config": driftRecordedObject(t, driftRunnerConfigFixture),
	}
}

// TestTownClusterDriftReportsEachRunnerDifference moves one field of the
// recorded live Deployment in the render, so the compare must name the grace
// period, the strategy and an argument by path.
func TestTownClusterDriftReportsEachRunnerDifference(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		moved    func(t *testing.T, rendered map[string]any)
		path     string
		rendered any
		// observed is the recorded live value at the path, read from the record
		// rather than a template constant, so the case does not couple to one.
		observed func(t *testing.T, live map[string]any) any
	}{
		{
			name: "a grace period",
			moved: func(_ *testing.T, rendered map[string]any) {
				driftPodSpec(rendered)["terminationGracePeriodSeconds"] = 60
			},
			path:     "spec.template.spec.terminationGracePeriodSeconds",
			rendered: 60,
			observed: func(_ *testing.T, live map[string]any) any {
				return driftPodSpec(live)["terminationGracePeriodSeconds"]
			},
		},
		{
			name: "a strategy",
			moved: func(_ *testing.T, rendered map[string]any) {
				driftObjectAt(rendered, "spec", "strategy")["type"] = "RollingUpdate"
			},
			path:     "spec.strategy.type",
			rendered: "RollingUpdate",
			observed: func(_ *testing.T, live map[string]any) any {
				return driftObjectAt(live, "spec", "strategy")["type"]
			},
		},
		{
			name: "an argument",
			moved: func(t *testing.T, rendered map[string]any) {
				driftInitContainer(t, rendered, "dind")["args"] = []any{"--insecure-registry=forgejo:3999"}
			},
			path:     "spec.template.spec.initContainers[name=dind].args[0]",
			rendered: "--insecure-registry=forgejo:3999",
			observed: func(t *testing.T, live map[string]any) any {
				return driftInitContainer(t, live, "dind")["args"].([]any)[0]
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, town := driftTestSeams(t, nil)
			m := driftTestRendered(t, d, driftTestFlags(t, town))
			live := driftRecordedObject(t, driftRunnerDeploymentFixture)
			rendered := driftRecordedObject(t, driftRunnerDeploymentFixture)
			tc.moved(t, rendered)
			targets := runnerDriftTargets(mustBuildRunner(t, driftControlRunner(t, "")))
			for i := range targets {
				if targets[i].kind == "Deployment" {
					targets[i].object = rendered
				}
			}
			d.runner = func() ([]driftTarget, error) { return targets, nil }
			d.kubectl = driftRecordedAnswer(t, driftAnswering(t, m, nil), map[string]map[string]any{
				"deployment/" + forgejorunner.Name: live,
			})
			err := runClusterDrift(context.Background(), d)
			if got := driftExit(err); got != 1 {
				t.Fatalf("exit = %d, want 1 (err: %v); printed:\n%s", got, err, out.String())
			}
			wantLines := []string{"deployment/" + forgejorunner.Name + ": " + driftWantDifference(t, tc.path, tc.rendered, tc.observed(t, live))}
			if got := driftLines(out); !slices.Equal(got, wantLines) {
				t.Errorf("printed\n%q\nwant\n%q", got, wantLines)
			}
		})
	}
}

// mustBuildRunner renders v's runner manifests, failing the test when they
// cannot be built.
func mustBuildRunner(t *testing.T, v forgejorunner.Values) forgejorunner.Manifests {
	t.Helper()
	m, err := forgejorunner.Build(v)
	if err != nil {
		t.Fatalf("rendering the runner: %v", err)
	}
	return m
}

// TestTownClusterDriftReportsTheRunnersConfig holds a recorded ConfigMap whose
// config.yml differs to one line naming data.config.yml and a fingerprint of
// each side, with the document's text nowhere in the output.
func TestTownClusterDriftReportsTheRunnersConfig(t *testing.T) {
	t.Parallel()
	d, out, town := driftTestSeams(t, nil)
	m := driftTestRendered(t, d, driftTestFlags(t, town))
	renderedConfig := mustBuildRunner(t, driftControlRunner(t, "")).ConfigMap.Data["config.yml"]
	live := driftRecordedObject(t, driftRunnerConfigFixture)
	recordedConfig := live["data"].(map[string]any)["config.yml"].(string)
	liveConfig := strings.Replace(recordedConfig, "capacity: 2", "capacity: 3", 1)
	if liveConfig == recordedConfig {
		t.Fatal("fixture: the recorded config.yml names no capacity of 2")
	}
	live["data"].(map[string]any)["config.yml"] = liveConfig
	d.kubectl = driftRecordedAnswer(t, driftAnswering(t, m, nil), map[string]map[string]any{
		"configmap/" + forgejorunner.Name + "-config": live,
	})
	err := runClusterDrift(context.Background(), d)
	if got := driftExit(err); got != 1 {
		t.Fatalf("exit = %d, want 1 (err: %v); printed:\n%s", got, err, out.String())
	}
	wantLines := []string{"configmap/" + forgejorunner.Name + "-config: " + driftWantDifference(t, "data.config.yml", renderedConfig, liveConfig)}
	if got := driftLines(out); !slices.Equal(got, wantLines) {
		t.Errorf("printed\n%q\nwant\n%q", got, wantLines)
	}
	if strings.Contains(out.String(), "capacity") {
		t.Errorf("printed the config.yml text, want paths and fingerprints only:\n%s", out.String())
	}
	for _, line := range driftLines(out) {
		if !driftLineShape.MatchString(line) {
			t.Errorf("line %q is not a path and two fingerprints", line)
		}
	}
}

// TestTownClusterDriftRefusesAnUnreadableGateRunner holds the runner Deployment
// and ConfigMap the cluster does not hold to the unreadable exit status, never 0
// or 1.
func TestTownClusterDriftRefusesAnUnreadableGateRunner(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		resource string
	}{
		{name: "the runner is absent", resource: "deployment/" + forgejorunner.Name},
		{name: "the runner's config is absent", resource: "configmap/" + forgejorunner.Name + "-config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, town := driftTestSeams(t, nil)
			answer := driftAnswering(t, driftTestRendered(t, d, driftTestFlags(t, town)), nil)
			d.kubectl = func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
				if len(args) > 1 && args[1] == tc.resource {
					// kubectl's status for an object the cluster does not hold.
					return "", 1, nil
				}
				return answer(ctx, dir, env, name, args...)
			}
			err := runClusterDrift(context.Background(), d)
			if got := driftExit(err); got != driftExitUnreadable {
				t.Fatalf("exit = %d, want %d (err: %v); printed:\n%s", got, driftExitUnreadable, err, out.String())
			}
			if want := "the live " + tc.resource + " could not be read"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not hold %q", err, want)
			}
			if got := driftLines(out); got != nil {
				t.Errorf("printed %q beside the refusal, want nothing", got)
			}
		})
	}
}

// TestTownClusterDriftRefusesARunnerItCannotRender holds a runner render that
// refuses to the unreadable exit status, before any cluster read.
func TestTownClusterDriftRefusesARunnerItCannotRender(t *testing.T) {
	t.Parallel()
	d, out, _ := driftTestSeams(t, nil)
	d.runner = func() ([]driftTarget, error) {
		return nil, errors.New("values.control.yaml: capacity: want a job count of zero or more")
	}
	d.kubectl = driftNoRead(t)
	err := runClusterDrift(context.Background(), d)
	if got := driftExit(err); got != driftExitUnreadable {
		t.Fatalf("exit = %d, want %d (err: %v); printed:\n%s", got, driftExitUnreadable, err, out.String())
	}
	if want := "cannot render the gate runner to compare"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not hold %q", err, want)
	}
	if got := driftLines(out); got != nil {
		t.Errorf("printed %q beside the refusal, want nothing", got)
	}
}

// TestTownClusterDriftRefusesADepsWithoutTheRunnerRender holds a deps value that
// never got its runner seam to the unreadable exit status: a run that cannot
// render the runner is never a cluster in step, and it does not panic.
func TestTownClusterDriftRefusesADepsWithoutTheRunnerRender(t *testing.T) {
	t.Parallel()
	d, out, _ := driftTestSeams(t, nil)
	d.runner = nil
	d.kubectl = driftNoRead(t)
	err := runClusterDrift(context.Background(), d)
	if got := driftExit(err); got != driftExitUnreadable {
		t.Fatalf("exit = %d, want %d (err: %v); printed:\n%s", got, driftExitUnreadable, err, out.String())
	}
	if want := "no render of the gate runner is wired"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not hold %q", err, want)
	}
}

// TestRunnerDriftTargetsRefusesAValueOrBuildItCannotRead holds the runner's
// targets to both refusals: values that cannot be decoded and a build that
// refuses are errors, never a target list the compare reads as a cluster in
// step.
func TestRunnerDriftTargetsRefusesAValueOrBuildItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		values func() (forgejorunner.Values, error)
		build  func(forgejorunner.Values) (forgejorunner.Manifests, error)
		want   string
	}{
		{
			name: "the values cannot be read",
			values: func() (forgejorunner.Values, error) {
				return forgejorunner.Values{}, errors.New("values.control.yaml: capacity: want a job count of zero or more")
			},
			build: forgejorunner.Build,
			want:  "reading the control cluster's runner values",
		},
		{
			name:   "the render refuses",
			values: forgejorunner.ControlValues,
			build: func(forgejorunner.Values) (forgejorunner.Manifests, error) {
				return forgejorunner.Manifests{}, errors.New("the dind share is empty")
			},
			want: "rendering the runner",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			targets, err := runnerDriftTargetsFrom(tc.values, tc.build)
			if err == nil {
				t.Fatalf("targets = %d with no error, want the refusal", len(targets))
			}
			if targets != nil {
				t.Errorf("targets = %v beside the error, want none", targets)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
		})
	}
}

// driftReadPlurals maps a drift read's kind, before the slash, to its granted resource.
var driftReadPlurals = map[string]string{
	"deployment": "deployments",
	"configmap":  "configmaps",
}

// TestTownClusterDriftReadsMatchTheGolden derives the compare's reads from the
// run itself and holds the set to the golden the ClusterRole is checked against.
func TestTownClusterDriftReadsMatchTheGolden(t *testing.T) {
	t.Parallel()
	d, _, town := driftFourDeploymentSeams(t)
	var calls []string
	d.kubectl = driftRecordReads(driftAnswering(t, driftTestRendered(t, d, driftTestFlags(t, town)), nil), &calls)
	if err := compareClusterDrift(context.Background(), d); err != nil {
		t.Fatalf("comparing the render with its live form: %v", err)
	}
	read := map[string]bool{}
	for _, call := range calls {
		plural, known := driftReadPlurals[strings.SplitN(call, "/", 2)[0]]
		if !known {
			t.Fatalf("the compare reads %q, which driftReadPlurals does not map; add the kind and a golden line", call)
		}
		read[plural] = true
	}
	raw, err := os.ReadFile(filepath.FromSlash("testdata/drift-reads.golden"))
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	golden := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Fields(line); len(fields) == 2 {
			golden[fields[1]] = true
		} else {
			t.Fatalf("golden line %q is not `<verb> <resource>`", line)
		}
	}
	if got, want := slices.Sorted(maps.Keys(read)), slices.Sorted(maps.Keys(golden)); !slices.Equal(got, want) {
		t.Errorf("the compare reads %q, and the golden lists %q", got, want)
	}
}

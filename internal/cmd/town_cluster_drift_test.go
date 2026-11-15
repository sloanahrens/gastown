package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
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
	return clusterDriftDeps{render: clusterRenderDepsAt(t, town, io.Discard), out: out}, out, town
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

// driftAnswering answers each kubectl read with the live form of the Deployment
// the run rendered, then applies mutate to it.
func driftAnswering(t *testing.T, m k8sdaemon.Manifests, mutate func(*testing.T, map[string]any)) update.RunFunc {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
		if name != "kubectl" {
			t.Fatalf("ran %q, want kubectl", name)
		}
		if len(args) < 2 || args[0] != "get" || args[len(args)-1] != "json" {
			t.Fatalf("ran kubectl %q, want a get with -o json", args)
		}
		var deployment k8sdaemon.Deployment
		switch resource := args[1]; resource {
		case "deployment/" + k8sdaemon.Name:
			deployment = m.Deployment
		case "deployment/" + k8sdaemon.DashboardName:
			deployment = m.DashboardDeployment
		default:
			t.Fatalf("kubectl read %q, which the run did not render", resource)
		}
		return driftLiveAnswer(t, deployment, mutate), 0, nil
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

// driftLiveAnswer returns the rendered Deployment as kubectl reports the live
// one: a JSON round trip, so its numbers are float64 as encoding/json makes them,
// plus the fields and defaults the API server adds, then mutate.
func driftLiveAnswer(t *testing.T, rendered k8sdaemon.Deployment, mutate func(*testing.T, map[string]any)) string {
	t.Helper()
	live := driftDecodedDeployment(t, rendered)
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

// driftDecodedDeployment decodes a rendered Deployment to the field tree
// encoding/json makes of the same object.
func driftDecodedDeployment(t *testing.T, rendered k8sdaemon.Deployment) map[string]any {
	t.Helper()
	var body bytes.Buffer
	if err := k8sdaemon.YAML(&body, rendered); err != nil {
		t.Fatalf("encoding the rendered Deployment: %v", err)
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(body.Bytes(), &decoded); err != nil {
		t.Fatalf("decoding the rendered Deployment: %v", err)
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
// applied: the metadata it stamps, the defaults it sets, and a status.
func driftServerFields(live map[string]any) {
	metadata := driftObjectAt(live, "metadata")
	metadata["generation"] = 4
	metadata["resourceVersion"] = "12345"
	metadata["uid"] = "5d0e6a2c-0000-4000-8000-000000000000"
	metadata["creationTimestamp"] = "2026-11-15T10:00:00Z"
	driftObjectAt(metadata, "annotations")["deployment.kubernetes.io/revision"] = "4"

	templateMeta := driftTemplateMeta(live)
	templateMeta["creationTimestamp"] = nil
	driftObjectAt(templateMeta, "annotations")[restartedAtAnnotation] = "2026-11-15T10:30:00Z"

	podSpec := driftPodSpec(live)
	podSpec["dnsPolicy"] = "ClusterFirst"
	podSpec["restartPolicy"] = "Always"

	spec := driftObjectAt(live, "spec")
	spec["progressDeadlineSeconds"] = 600
	spec["revisionHistoryLimit"] = 10

	for _, entry := range driftContainers(live) {
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
				`deployment/daemon: spec.template.spec.containers[name=daemon].env[name=TZ].value: rendered "America/Chicago", live "UTC"`,
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
				`deployment/dashboard: spec.template.spec.containers[name=dashboard].env[name=TZ].value: rendered "America/Chicago", live "UTC"`,
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
// failed, a nonzero or an unreadable kubectl answer being an error, never a
// cluster in step.
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
			wantExit: 1,
			want:     "reading the live deployment/daemon",
		},
		{
			name: "kubectl exited nonzero",
			answer: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "", 1, nil
			},
			wantExit: 1,
			want:     "kubectl exited 1",
		},
		{
			name: "the answer is not JSON",
			answer: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "<html>", 0, nil
			},
			wantExit: 1,
			want:     "not a JSON object",
		},
		{
			name: "the answer is a list",
			answer: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "[]", 0, nil
			},
			wantExit: 1,
			want:     "not a JSON object",
		},
		{
			name: "the live object has no fields",
			answer: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "{}", 0, nil
			},
			wantExit: 1,
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

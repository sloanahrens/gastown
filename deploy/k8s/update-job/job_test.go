package updatejob

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// updatingGoldens reports whether -update was passed. The flag is registered in
// TestMain, so a run without it compares the goldens and a change to the rendered
// output fails loudly.
func updatingGoldens() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

// goldenValues is the configuration the goldens record.
func goldenValues() Values {
	return Values{
		Image:       "registry.example.test/gastown/daemon@sha256:" + strings.Repeat("0123456789abcdef", 4),
		GreenCommit: strings.Repeat("ab", 20),
		MaxRuntime:  2 * time.Hour,
	}
}

func mustBuild(t *testing.T, v Values) Manifests {
	t.Helper()
	m, err := Build(v)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return m
}

// compareGolden compares got with testdata/file, or rewrites it when -update is set.
func compareGolden(t *testing.T, file string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", file)
	if updatingGoldens() {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s does not match the renderer; re-run with -update if the change is intended\n--- %s\n%s--- renderer\n%s", path, path, want, got)
	}
}

// podOf returns the Job's pod spec, the one the CronJob's template must equal.
func podOf(m Manifests) PodSpec {
	return m.Job.Spec.Template.Spec
}

func containerNamed(t *testing.T, pod PodSpec, name string) Container {
	t.Helper()
	for _, c := range pod.InitContainers {
		if c.Name == name {
			return c
		}
	}
	for _, c := range pod.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the pod has no container %q", name)
	return Container{}
}

func TestRenderJobGolden(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues())
	var buf bytes.Buffer
	if err := YAML(&buf, m.Job); err != nil {
		t.Fatalf("YAML: %v", err)
	}
	compareGolden(t, "job.yaml", buf.Bytes())
}

func TestRenderCronJobGolden(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues())
	var buf bytes.Buffer
	if err := YAML(&buf, m.CronJob); err != nil {
		t.Fatalf("YAML: %v", err)
	}
	compareGolden(t, "cronjob.yaml", buf.Bytes())
}

// TestCronJobIsSuspendedAndSharesTheJobPodTemplate holds the CronJob to the Job: it
// is suspended, and its job template's spec encodes to the same bytes as the Job's
// spec, so the daemon's Jobs and the hand run's Job are one template.
func TestCronJobIsSuspendedAndSharesTheJobPodTemplate(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues())
	if !m.CronJob.Spec.Suspend {
		t.Error("the CronJob is not suspended, so it would fire on its schedule")
	}
	jobPod, err := yaml.Marshal(m.Job.Spec)
	if err != nil {
		t.Fatalf("marshal Job spec: %v", err)
	}
	cronPod, err := yaml.Marshal(m.CronJob.Spec.JobTemplate.Spec)
	if err != nil {
		t.Fatalf("marshal CronJob job template: %v", err)
	}
	if !bytes.Equal(jobPod, cronPod) {
		t.Errorf("the CronJob's job template differs from the Job's spec\n--- job\n%s--- cronjob\n%s", jobPod, cronPod)
	}
}

// TestPodRunsAsTheUpdateAccountAtControlPlanePriority holds the pod-level settings the
// roll depends on: its ServiceAccount, its priority, the one attempt, the run bound
// taken from Values.MaxRuntime, and the day the finished Job is kept.
func TestPodRunsAsTheUpdateAccountAtControlPlanePriority(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues())
	pod := podOf(m)
	if pod.ServiceAccountName != "gt-update" {
		t.Errorf("serviceAccountName = %q, want gt-update", pod.ServiceAccountName)
	}
	if pod.AutomountServiceAccountToken == nil || !*pod.AutomountServiceAccountToken {
		t.Error("the pod does not mount its ServiceAccount token, so kubectl has no in-cluster credential")
	}
	if pod.PriorityClassName != "gt-control-plane" {
		t.Errorf("priorityClassName = %q, want gt-control-plane", pod.PriorityClassName)
	}
	if pod.RestartPolicy != "Never" {
		t.Errorf("restartPolicy = %q, want Never", pod.RestartPolicy)
	}
	if m.Job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %d, want 0", m.Job.Spec.BackoffLimit)
	}
	if want := int64(2 * 60 * 60); m.Job.Spec.ActiveDeadlineSeconds != want {
		t.Errorf("activeDeadlineSeconds = %d, want %d from MaxRuntime 2h", m.Job.Spec.ActiveDeadlineSeconds, want)
	}
	if m.Job.Spec.TTLSecondsAfterFinished != 86400 {
		t.Errorf("ttlSecondsAfterFinished = %d, want 86400", m.Job.Spec.TTLSecondsAfterFinished)
	}
}

// TestInitRecordsTheImagesAndTheContainerRollsThem holds the two commands. Both run
// the Values.Image digest with gt, in the town root, and the values file is the one
// path both name.
func TestInitRecordsTheImagesAndTheContainerRollsThem(t *testing.T) {
	t.Parallel()
	v := goldenValues()
	pod := podOf(mustBuild(t, v))
	images := containerNamed(t, pod, "images")
	update := containerNamed(t, pod, "update")
	for _, c := range []Container{images, update} {
		if c.Image != v.Image {
			t.Errorf("%s image = %q, want the digest %q", c.Name, c.Image, v.Image)
		}
		if !slices.Equal(c.Command, []string{"gt"}) {
			t.Errorf("%s command = %v, want [gt]", c.Name, c.Command)
		}
		if c.WorkingDir != k8sdaemon.TownRootPath {
			t.Errorf("%s workingDir = %q, want the town root %q", c.Name, c.WorkingDir, k8sdaemon.TownRootPath)
		}
	}
	wantImages := []string{"update", "images", "--sha", v.GreenCommit, "--out", ValuesPath}
	if !slices.Equal(images.Args, wantImages) {
		t.Errorf("images args = %v, want %v", images.Args, wantImages)
	}
	wantUpdate := []string{"update", "--images", ValuesPath, "--pod-in-cluster", "--pod-namespace", "gt"}
	if !slices.Equal(update.Args, wantUpdate) {
		t.Errorf("update args = %v, want %v", update.Args, wantUpdate)
	}
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != "images" {
		t.Errorf("init containers = %+v, want the one images container, so the roll waits for the record", pod.InitContainers)
	}
}

// TestJobGitCredentialEnv holds git's credential helper to the viewer token and the
// Forgejo Service: both the init container and the main container carry the three
// GIT_CONFIG variables, and the helper line names the mounted token file and the
// host the daemon's constants compose.
func TestJobGitCredentialEnv(t *testing.T) {
	t.Parallel()
	host := k8sdaemon.ForgejoName + ":" + strconv.Itoa(k8sdaemon.ForgejoPort)
	// The goldens carry this host too: a change to the daemon's Service name or
	// port is a deliberate change to the Job's helper line, and this check names
	// the change instead of letting the two drift.
	const wantHost = "forgejo:3000"
	if host != wantHost {
		t.Fatalf("the daemon's Forgejo name and port compose %q, want %q", host, wantHost)
	}
	pod := podOf(mustBuild(t, goldenValues()))
	want := []EnvVar{
		{Name: "GIT_CONFIG_COUNT", Value: "1"},
		{Name: "GIT_CONFIG_KEY_0", Value: "credential.http://" + host + ".helper"},
		{Name: "GIT_CONFIG_VALUE_0", Value: "!gt git-credential-forgejo --host " + host + " --token-file " + ViewerTokenPath},
	}
	for _, name := range []string{InitContainerName, ContainerName} {
		c := containerNamed(t, pod, name)
		for _, w := range want {
			got, ok := envNamed(c.Env, w.Name)
			if !ok {
				t.Errorf("%s carries no %s, so git has no credential helper and the main-tip read asks for a login it cannot answer", c.Name, w.Name)
				continue
			}
			if got.Value != w.Value {
				t.Errorf("%s %s = %q, want %q", c.Name, w.Name, got.Value, w.Value)
			}
		}
	}
}

// envNamed returns the environment variable named name.
func envNamed(env []EnvVar, name string) (EnvVar, bool) {
	for _, e := range env {
		if e.Name == name {
			return e, true
		}
	}
	return EnvVar{}, false
}

// TestOnlyTheTownStateAndTheViewerTokenAreMounted holds the pod's volumes to the two
// the roll needs: the town-state claim, and the viewer token Secret the dashboard
// mounts. Every mount of every container is one of those two, so no other secret
// reaches the pod.
func TestOnlyTheTownStateAndTheViewerTokenAreMounted(t *testing.T) {
	t.Parallel()
	pod := podOf(mustBuild(t, goldenValues()))
	if len(pod.Volumes) != 2 {
		t.Fatalf("the pod has %d volumes, want the town-state claim and the viewer token", len(pod.Volumes))
	}
	state := volumeNamed(t, pod.Volumes, TownStateVolume)
	if state.PersistentVolumeClaim == nil || state.PersistentVolumeClaim.ClaimName != k8sdaemon.TownStateClaim || state.Secret != nil {
		t.Errorf("town-state volume = %+v, want the %s claim", state, k8sdaemon.TownStateClaim)
	}
	viewer := volumeNamed(t, pod.Volumes, ViewerTokenVolume)
	if viewer.Secret == nil || viewer.Secret.SecretName != "daemon-forgejo-viewer.env" || viewer.PersistentVolumeClaim != nil {
		t.Errorf("viewer-token volume = %+v, want the daemon-forgejo-viewer.env Secret and nothing else", viewer)
	}
	if viewer.Secret != nil && (viewer.Secret.DefaultMode == nil || *viewer.Secret.DefaultMode != ViewerTokenMode) {
		t.Errorf("viewer token defaultMode = %v, want %o", viewer.Secret.DefaultMode, ViewerTokenMode)
	}
	for _, c := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
		for _, mount := range c.VolumeMounts {
			if mount.Name != TownStateVolume && mount.Name != ViewerTokenVolume {
				t.Errorf("%s mounts %q, which is neither the town state nor the viewer token", c.Name, mount.Name)
			}
			if mount.Name == ViewerTokenVolume && (!mount.ReadOnly || mount.SubPath != k8sdaemon.ViewerTokenFile || mount.MountPath != ViewerTokenPath) {
				t.Errorf("%s mounts the viewer token as %+v, want read-only at %s from %s", c.Name, mount, ViewerTokenPath, k8sdaemon.ViewerTokenFile)
			}
			if mount.Name == TownStateVolume && mount.MountPath != k8sdaemon.TownRootPath {
				t.Errorf("%s mounts the town state at %q, want %q", c.Name, mount.MountPath, k8sdaemon.TownRootPath)
			}
		}
	}
}

// TestBuildRefusesWhatThePodCannotRun holds each refusal to a row that names the
// value it refuses, so a dropped check fails the row that needed it.
func TestBuildRefusesWhatThePodCannotRun(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		name   string
		edit   func(v *Values)
		wantIn string
	}{
		{name: "no image", edit: func(v *Values) { v.Image = "" }, wantIn: "Values.Image"},
		{name: "a tag, not a digest", edit: func(v *Values) { v.Image = "registry.example.test/gastown/daemon:latest" }, wantIn: "Values.Image"},
		{name: "a digest with an upper-case hex", edit: func(v *Values) { v.Image = "r/d@sha256:" + strings.Repeat("A", 64) }, wantIn: "Values.Image"},
		{name: "a namespace with an underscore", edit: func(v *Values) { v.Namespace = "gt_update" }, wantIn: "Values.Namespace"},
		{name: "a namespace with capitals", edit: func(v *Values) { v.Namespace = "GT" }, wantIn: "Values.Namespace"},
		{name: "a namespace that starts with a dash", edit: func(v *Values) { v.Namespace = "-gt" }, wantIn: "Values.Namespace"},
		{name: "a short commit", edit: func(v *Values) { v.GreenCommit = "4a01ba03" }, wantIn: "Values.GreenCommit"},
		{name: "an upper-case commit", edit: func(v *Values) { v.GreenCommit = strings.Repeat("AB", 20) }, wantIn: "Values.GreenCommit"},
		{name: "no run bound", edit: func(v *Values) { v.MaxRuntime = 0 }, wantIn: "Values.MaxRuntime"},
		{name: "a negative run bound", edit: func(v *Values) { v.MaxRuntime = -time.Minute }, wantIn: "Values.MaxRuntime"},
		{name: "a fractional second", edit: func(v *Values) { v.MaxRuntime = 1500 * time.Millisecond }, wantIn: "whole number of seconds"},
	} {
		v := goldenValues()
		r.edit(&v)
		_, err := Build(v)
		if err == nil {
			t.Errorf("%s: Build accepted the values, want a refusal naming %s", r.name, r.wantIn)
			continue
		}
		if !strings.Contains(err.Error(), r.wantIn) {
			t.Errorf("%s: error %q does not name %s", r.name, err, r.wantIn)
		}
	}
}

// TestNamespaceDefaultsToTheDaemonNamespace holds an empty Namespace to the namespace
// the daemon runs in, so the Job and the CronJob land beside the Deployments they move.
func TestNamespaceDefaultsToTheDaemonNamespace(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues())
	if m.Job.Metadata.Namespace != "gt" || m.CronJob.Metadata.Namespace != "gt" {
		t.Errorf("namespaces = %q and %q, want gt for both", m.Job.Metadata.Namespace, m.CronJob.Metadata.Namespace)
	}
}

// volumeNamed returns the volume named name, failing the test when none is.
func volumeNamed(t *testing.T, volumes []Volume, name string) Volume {
	t.Helper()
	for _, v := range volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("the pod has no volume %q", name)
	return Volume{}
}

// failingWriter is a writer whose every write fails, so a test can drive the write
// refusal of YAML.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// refusingValue is a value whose marshal fails, so a test can drive the encode refusal
// of YAML. The renderer's own types always encode, so no real Values reaches it.
type refusingValue struct{}

func (refusingValue) MarshalYAML() (any, error) { return nil, errors.New("refused") }

// TestYAMLRefusesWhatCannotEncodeAndWritesNothing holds YAML to its two refusals: a
// value that cannot be encoded is refused with no byte written, and a write that fails
// is reported rather than dropped.
func TestYAMLRefusesWhatCannotEncodeAndWritesNothing(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := YAML(&buf, refusingValue{}); err == nil || !strings.Contains(err.Error(), "encoding the manifest") {
		t.Errorf("YAML of a refusing value: err = %v, want an encoding refusal", err)
	}
	if buf.Len() != 0 {
		t.Errorf("YAML of a refusing value wrote %d bytes, want none", buf.Len())
	}
	if err := YAML(failingWriter{}, Manifests{}.Job); err == nil || !strings.Contains(err.Error(), "writing the manifest") {
		t.Errorf("YAML into a failing writer: err = %v, want a writing refusal", err)
	}
}

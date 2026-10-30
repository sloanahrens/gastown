package sweeprunner

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// k8sNamePattern is the label rule the Kubernetes API applies to an object's
// name, spelled out in the test rather than taken from the code it checks.
var k8sNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// TestK8sRenderGolden renders one fixed Spec and compares it to the checked-in
// Job, so any change to the document is a change the reviewer sees.
func TestK8sRenderGolden(t *testing.T) {
	t.Parallel()
	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	if plan.Job == nil {
		t.Fatal("the Kubernetes plan renders no Job")
	}
	got := string(plan.Job.JSON())
	golden, err := os.ReadFile(filepath.Join("testdata", "k8s-render.golden.json"))
	if err != nil {
		t.Fatalf("reading the golden file: %v\nrendered:\n%s", err, got)
	}
	if got != string(golden) {
		t.Errorf("rendered Job differs from testdata/k8s-render.golden.json\ngot:\n%s\nwant:\n%s", got, golden)
	}

	runner, ok := containerNamed(plan.Job, runnerContainerName)
	if !ok {
		t.Fatalf("the Job renders no %q container", runnerContainerName)
	}
	if runner.Resources == nil {
		t.Fatalf("the %q container renders no resources, so the run has no share", runnerContainerName)
	}
	if requests, limits := runner.Resources.Requests, runner.Resources.Limits; requests != limits {
		t.Errorf("the runner's requests %+v differ from its limits %+v: the share must be promised, not burst", requests, limits)
	}
	spec := goldenSpec()
	share := ResourceList{CPU: strconv.Itoa(spec.CPUs), Memory: strconv.Itoa(spec.MemoryGB) + "Gi"}
	if got := runner.Resources.Requests; got != share {
		t.Errorf("the runner's requests are %+v, want the Spec's share %+v", got, share)
	}
}

// TestK8sRenderPriorityAndSafety holds the Job to the cluster's rules: it runs at
// the CI priority, one attempt with no restart, off a read-only repo claim into
// its own output claim, with no host path and no privileged container but the
// dind sidecar, which is bounded and bound to the pod's loopback.
func TestK8sRenderPriorityAndSafety(t *testing.T) {
	t.Parallel()
	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	if plan.Job == nil {
		t.Fatal("the Kubernetes plan renders no Job")
	}
	pod := plan.Job.Spec.Template.Spec
	if got, want := pod.PriorityClassName, "gt-ci"; got != want {
		t.Errorf("priorityClassName = %q, want the class %q deploy/k8s/priority-classes.yaml defines for CI", got, want)
	}
	if got := pod.RestartPolicy; got != "Never" {
		t.Errorf("restartPolicy = %q, want Never: a run reports its own failure once", got)
	}
	if backoff := plan.Job.Spec.BackoffLimit; backoff == nil || *backoff != 0 {
		t.Errorf("backoffLimit = %v, want 0: a failed run is not retried", backoff)
	}
	if document := string(plan.Job.JSON()); strings.Contains(document, "hostPath") {
		t.Errorf("the Job mounts a host path, which ties the run to one node:\n%s", document)
	}
	for _, volume := range pod.Volumes {
		if volume.PersistentVolumeClaim == nil && volume.EmptyDir == nil {
			t.Errorf("volume %q names no source", volume.Name)
		}
	}
	if got, want := privilegedContainers(plan.Job), []string{dindContainerName}; !slices.Equal(got, want) {
		t.Errorf("privileged containers = %v, want only the dind sidecar %v", got, want)
	}
	dind, ok := containerNamed(plan.Job, dindContainerName)
	if !ok {
		t.Fatalf("the Job renders no %q container", dindContainerName)
	}
	if got, want := dind.Args, []string{"--host=tcp://127.0.0.1:" + dindPort, "--tls=false"}; !slices.Equal(got, want) {
		t.Errorf("dind args = %v, want %v: an engine bound to any other address answers at the pod IP, where every pod on the cluster can drive it", got, want)
	}
	if dind.Resources == nil {
		t.Fatalf("the %q container renders no resources, so the sidecar competes for the node's CPU and memory without a bound", dindContainerName)
	}
	if requests, limits := dind.Resources.Requests, dind.Resources.Limits; requests != limits {
		t.Errorf("dind requests %+v differ from its limits %+v: the sidecar's share is bounded like the runner's", requests, limits)
	}
	repo, ok := volumeMountNamed(plan.Job, repoVolume)
	if !ok {
		t.Fatalf("the Job mounts no %q volume", repoVolume)
	}
	if !repo.ReadOnly {
		t.Errorf("the repo mount is writable; a run reads the repository and never writes it")
	}
	out, ok := volumeMountNamed(plan.Job, outVolume)
	if !ok {
		t.Fatalf("the Job mounts no %q volume", outVolume)
	}
	if out.ReadOnly {
		t.Errorf("the output mount is read-only; the run writes result.json there")
	}

	t.Run("without dind", func(t *testing.T) {
		t.Parallel()
		spec := goldenSpec()
		spec.Dind = false
		plain, err := Kubernetes{}.Render(spec)
		if err != nil {
			t.Fatalf("rendering a spec without dind: %v", err)
		}
		if got := privilegedContainers(plain.Job); len(got) != 0 {
			t.Errorf("a run without dind renders privileged containers %v", got)
		}
		document := string(plain.Job.JSON())
		if strings.Contains(document, "privileged") {
			t.Errorf("a run without dind renders privileged:\n%s", document)
		}
		if _, ok := containerNamed(plain.Job, dindContainerName); ok {
			t.Errorf("a run without dind renders the %q sidecar", dindContainerName)
		}
		runner, ok := containerNamed(plain.Job, runnerContainerName)
		if !ok {
			t.Fatalf("the Job renders no %q container", runnerContainerName)
		}
		for _, env := range runner.Env {
			if env.Name == "DOCKER_HOST" {
				t.Errorf("a run without dind points the runner at an engine it did not start: %s=%s", env.Name, env.Value)
			}
		}
		for _, volume := range plain.Job.Spec.Template.Spec.Volumes {
			if volume.Name == dindVolume {
				t.Errorf("a run without dind renders the %q volume", dindVolume)
			}
		}
		if got := plain.Job.Spec.Template.Spec.PriorityClassName; got != "gt-ci" {
			t.Errorf("a run without dind has priorityClassName %q, want the CI class", got)
		}
	})
}

// TestK8sJobLifeIsBounded holds the Job to a deadline and a TTL, so a run that
// hangs gives its share back and a Job nobody tore down does not outlive its
// run.
func TestK8sJobLifeIsBounded(t *testing.T) {
	t.Parallel()
	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	if plan.Job == nil {
		t.Fatal("the Kubernetes plan renders no Job")
	}
	deadline := plan.Job.Spec.ActiveDeadlineSeconds
	if deadline == nil || *deadline <= 0 {
		t.Errorf("activeDeadlineSeconds = %v, want a positive ceiling: a pod hung on one package holds its share forever", deadline)
	}
	ttl := plan.Job.Spec.TTLSecondsAfterFinished
	if ttl == nil || *ttl <= 0 {
		t.Errorf("ttlSecondsAfterFinished = %v, want the finished Job removed by the cluster", ttl)
	}
}

// TestK8sPriorityClassIsDeclared holds the class the Job runs at to the class
// the cluster's manifest declares, so a rename there fails here rather than at
// apply time (deploy/k8s/priority-classes.yaml).
func TestK8sPriorityClassIsDeclared(t *testing.T) {
	t.Parallel()
	manifest, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "priority-classes.yaml"))
	if err != nil {
		t.Fatalf("reading the cluster's priority classes: %v", err)
	}
	classes := declaredPriorityClasses(string(manifest))
	if !slices.Contains(classes, PriorityClass) {
		t.Errorf("the Job runs at %q, which deploy/k8s/priority-classes.yaml does not declare; it declares %v", PriorityClass, classes)
	}
}

// priorityClassDecl is the manifest's `metadata: {name: <class>}` in the flow
// style the file writes it in.
var priorityClassDecl = regexp.MustCompile(`(?m)^metadata:\s*\{\s*name:\s*([a-z0-9-]+)`)

// declaredPriorityClasses returns the names the manifest declares, in file
// order.
func declaredPriorityClasses(manifest string) []string {
	var names []string
	for _, match := range priorityClassDecl.FindAllStringSubmatch(manifest, -1) {
		names = append(names, match[1])
	}
	return names
}

// TestBackendsShareValidation holds both backends to one table: every Spec the
// Docker backend refuses the Kubernetes one refuses with the same error text, so
// the engine cannot change which inputs a run accepts.
func TestBackendsShareValidation(t *testing.T) {
	t.Parallel()
	for _, row := range invalidSpecRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			spec := goldenSpec()
			row.set(&spec)
			_, dockerErr := Docker{}.Render(spec)
			_, k8sErr := Kubernetes{}.Render(spec)
			if dockerErr == nil || k8sErr == nil {
				t.Fatalf("docker error = %v, kubernetes error = %v; want both to refuse the Spec", dockerErr, k8sErr)
			}
			if dockerErr.Error() != k8sErr.Error() {
				t.Errorf("the backends refuse the Spec differently:\ndocker:     %v\nkubernetes: %v", dockerErr, k8sErr)
			}
			if !strings.Contains(k8sErr.Error(), "spec."+row.field) {
				t.Errorf("kubernetes error %q does not name spec.%s", k8sErr, row.field)
			}
		})
	}
}

// TestK8sNamesFitTheObjectRules holds every name the Job makes to the rule the
// API enforces, including for a rig and run id long enough to need cutting, and
// keeps two runs' names apart after the cut.
func TestK8sNamesFitTheObjectRules(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	spec.Rig = strings.Repeat("Rig_", 20)
	spec.RunID = strings.Repeat("run.", 15)
	plan, err := Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering a spec with long names: %v", err)
	}
	names := map[string]string{
		"Job":        plan.Job.Metadata.Name,
		"repo claim": RepoClaimName(spec.Rig),
		"out claim":  OutClaimName(spec),
	}
	for _, volume := range plan.Job.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			names[volume.Name+" volume"] = volume.PersistentVolumeClaim.ClaimName
		}
	}
	for what, name := range names {
		if len(name) > 63 || !k8sNamePattern.MatchString(name) {
			t.Errorf("the %s name %q is not a DNS-1123 label of at most 63 characters", what, name)
		}
	}

	other := spec
	other.RunID += "x"
	if JobName(other) == JobName(spec) {
		t.Errorf("two runs with different ids share the Job name %q", JobName(spec))
	}
	if OutClaimName(other) == OutClaimName(spec) {
		t.Errorf("two runs with different ids share the output claim %q", OutClaimName(spec))
	}

	// Ids a label cannot hold as written must not read the same once sanitised:
	// two runs that did would share a Job and an output claim, and one run's
	// teardown would remove the other's.
	for _, pair := range [][2]string{
		{"run.1", "run-1"},
		{"run_1", "run-1"},
		{"Run-1", "run-1"},
		{"-run-1", "run-1"},
		{"..", "-"},
	} {
		left, right := goldenSpec(), goldenSpec()
		left.RunID, right.RunID = pair[0], pair[1]
		if got := JobName(left); got == JobName(right) {
			t.Errorf("run ids %q and %q share the Job name %q", pair[0], pair[1], got)
		}
		if got := OutClaimName(left); got == OutClaimName(right) {
			t.Errorf("run ids %q and %q share the output claim %q", pair[0], pair[1], got)
		}
	}
}

// TestK8sRenderTeardownRemovesTheJobItRenders points the plan's removal at the
// objects the run made — the Job and the output claim — so a caller that applies
// the Job and stops still leaves nothing behind.
func TestK8sRenderTeardownRemovesTheJobItRenders(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	plan, err := Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	if got := len(plan.Steps) - len(plan.Teardown()); got != 0 {
		t.Errorf("the plan renders %d steps beside the teardown; the Job is the run's whole start", got)
	}
	teardown := plan.Teardown()
	if len(teardown) != 2 {
		t.Fatalf("the plan renders %d teardown steps, want the Job's removal and the output claim's: %v", len(teardown), teardown)
	}
	if !slices.Contains(teardown[0].Argv, plan.Job.Metadata.Name) {
		t.Errorf("the teardown %v does not name the Job %q", teardown[0].Argv, plan.Job.Metadata.Name)
	}
	if !slices.Contains(teardown[0].Argv, "job") {
		t.Errorf("the teardown %v does not remove a Job", teardown[0].Argv)
	}
	if !slices.Contains(teardown[1].Argv, OutClaimName(spec)) {
		t.Errorf("the teardown %v does not name the output claim %q, so every run's claim is left behind", teardown[1].Argv, OutClaimName(spec))
	}
	if !slices.Contains(teardown[1].Argv, "persistentvolumeclaim") {
		t.Errorf("the teardown %v does not remove a claim", teardown[1].Argv)
	}
}

// containerNamed returns the pod's container called name.
func containerNamed(job *Job, name string) (Container, bool) {
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name == name {
			return container, true
		}
	}
	return Container{}, false
}

// privilegedContainers returns the names of the pod's privileged containers, in
// the order the pod lists them.
func privilegedContainers(job *Job) []string {
	var names []string
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.SecurityContext != nil && container.SecurityContext.Privileged {
			names = append(names, container.Name)
		}
	}
	return names
}

// volumeMountNamed returns the runner's mount of the volume called name.
func volumeMountNamed(job *Job, name string) (VolumeMount, bool) {
	runner, ok := containerNamed(job, runnerContainerName)
	if !ok {
		return VolumeMount{}, false
	}
	for _, mount := range runner.VolumeMounts {
		if mount.Name == name {
			return mount, true
		}
	}
	return VolumeMount{}, false
}

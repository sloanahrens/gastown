package sweeprunner

import (
	"encoding/json"
	"github.com/steveyegge/gastown/internal/templates"
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

// TestRenderJobGoldenMatches renders one fixed Spec and compares it to the
// checked-in Job, so any change to the document is a change the reviewer sees.
func TestRenderJobGoldenMatches(t *testing.T) {
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
}

// TestRenderJobDindIsANativeSidecar holds the engine to a native sidecar: an
// init container with restartPolicy Always and an exec startup probe on the
// loopback port it binds, so the kubelet starts it before the runner and stops
// it when the runner exits. As a regular container it outlived the runner, the
// Job stayed Running and the run held its share until the deadline (gt-etbor).
func TestRenderJobDindIsANativeSidecar(t *testing.T) {
	t.Parallel()
	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	pod := plan.Job.Spec.Template.Spec
	var dind *Container
	for i := range pod.InitContainers {
		if pod.InitContainers[i].Name == dindContainerName {
			dind = &pod.InitContainers[i]
		}
	}
	if dind == nil {
		t.Fatalf("the Job's initContainers %+v do not hold the %q sidecar", pod.InitContainers, dindContainerName)
	}
	if dind.RestartPolicy != "Always" {
		t.Errorf("dind restartPolicy = %q, want Always: only a native sidecar starts before the runner and ends with the pod", dind.RestartPolicy)
	}
	if dind.StartupProbe == nil || dind.StartupProbe.Exec == nil {
		t.Fatalf("dind renders no exec startupProbe, so the runner races the engine's start")
	}
	wantCommand := []string{"docker", "-H", "tcp://" + dindAddress + ":" + dindPort, "info"}
	if got := dind.StartupProbe.Exec.Command; !slices.Equal(got, wantCommand) {
		t.Errorf("dind startupProbe runs %v, want %v: it must ask the engine on the sidecar's loopback", got, wantCommand)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Name != runnerContainerName {
		t.Errorf("the Job's containers are %+v, want only %q", pod.Containers, runnerContainerName)
	}
}

// TestRenderJobStartupProbeRunsInsideThePod holds the engine's startup probe to
// an exec action, which the kubelet runs inside the dind container beside
// dockerd. A tcpSocket or httpGet probe is dialed by the kubelet from the node's
// network namespace, where the engine's loopback is not listening, so it would
// never succeed and the runner would never start (gt-etbor).
func TestRenderJobStartupProbeRunsInsideThePod(t *testing.T) {
	t.Parallel()
	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	dind, ok := containerNamed(plan.Job, dindContainerName)
	if !ok {
		t.Fatalf("the Job renders no %q container", dindContainerName)
	}
	if dind.StartupProbe == nil || dind.StartupProbe.Exec == nil {
		t.Fatalf("the dind startupProbe is %+v, want an exec action run inside the container", dind.StartupProbe)
	}
	if len(dind.StartupProbe.Exec.Command) == 0 {
		t.Errorf("the dind startupProbe exec action runs no command")
	}
	document := string(plan.Job.JSON())
	for _, probe := range []string{`"tcpSocket"`, `"httpGet"`} {
		if strings.Contains(document, probe) {
			t.Errorf("the Job renders %s, which the kubelet dials from the node and never reaches the engine:\n%s", probe, document)
		}
	}
}

// TestRenderJobRunnerDoesNotWaitForSidecarExit holds Job completion to the
// runner: the pod's regular containers are the runner alone, so the Job reports
// Complete when the runner exits rather than waiting for an engine that never
// exits (gt-etbor).
func TestRenderJobRunnerDoesNotWaitForSidecarExit(t *testing.T) {
	t.Parallel()
	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	containers := plan.Job.Spec.Template.Spec.Containers
	if len(containers) != 1 || containers[0].Name != runnerContainerName {
		t.Fatalf("the pod's regular containers are %+v, want only %q: a regular %q container keeps the Job Running after the runner exits", containers, runnerContainerName, dindContainerName)
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
	if dind.Image != templates.SweepDindImage {
		t.Errorf("dind image = %q, want templates.SweepDindImage %q", dind.Image, templates.SweepDindImage)
	}
	if got, want := dind.Args, []string{"dockerd", "--host=tcp://127.0.0.1:" + dindPort, "--tls=false"}; !slices.Equal(got, want) {
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
		"sync pod":   syncPodName(spec),
		"reader pod": readerName(spec),
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
	for key, value := range plan.Job.Metadata.Labels {
		if len(value) > 63 || !k8sNamePattern.MatchString(value) {
			t.Errorf("the Job's %s label value %q is not a DNS-1123 label of at most 63 characters", key, value)
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

// TestTeardownRemovesTheSyncPodButNotTheRepoClaim holds the teardown to every
// object the run made and to the claim it must leave: the sync pod, the Job, the
// reader pod and the output claim are removed, and the repo claim is not,
// because every run of the rig reuses it.
func TestTeardownRemovesTheSyncPodButNotTheRepoClaim(t *testing.T) {
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
	want := []struct {
		resource string
		name     string
	}{
		{"pod", syncPodName(spec)},
		{"job", JobName(spec)},
		{"pod", readerName(spec)},
		{"persistentvolumeclaim", OutClaimName(spec)},
	}
	if len(teardown) != len(want) {
		t.Fatalf("the plan renders %d teardown steps, want %d: %v", len(teardown), len(want), teardown)
	}
	for i, w := range want {
		argv := teardown[i].Argv
		if !slices.Contains(argv, "delete") || !slices.Contains(argv, w.resource) || !slices.Contains(argv, w.name) {
			t.Errorf("teardown %d runs %v, want the %s %q removed", i, argv, w.resource, w.name)
		}
		if slices.Contains(argv, RepoClaimName(spec.Rig)) {
			t.Errorf("teardown %d runs %v, which removes the repo claim %q every run reuses", i, argv, RepoClaimName(spec.Rig))
		}
	}
}

// TestRenderPlanSyncsTheRepoClaimFirst holds the launch to the sync stage's
// order: the repo claim is applied, a sync pod is applied and waited for, the
// host bare repository's contents are copied in, the copy is chowned to the
// runner, the sync pod is removed, and only then are the output claim and the
// Job applied.
func TestRenderPlanSyncsTheRepoClaimFirst(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	plan, err := Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	if len(plan.Apply) != 8 {
		t.Fatalf("the plan applies %d steps, want the repo claim, the sync pod, its Ready wait, the copy, the chown, its removal, the output claim and the Job: %v", len(plan.Apply), plan.Apply)
	}

	var repoClaim Claim
	if err := json.Unmarshal(plan.Apply[0].Stdin, &repoClaim); err != nil {
		t.Fatalf("the first apply is not a document: %v", err)
	}
	if repoClaim.Kind != "PersistentVolumeClaim" || repoClaim.Metadata.Name != RepoClaimName(spec.Rig) {
		t.Errorf("the first apply is a %s %q, want the repo claim %q", repoClaim.Kind, repoClaim.Metadata.Name, RepoClaimName(spec.Rig))
	}
	if !slices.Equal(repoClaim.Spec.AccessModes, []string{"ReadWriteOnce"}) {
		t.Errorf("the repo claim's access modes are %v, want one read-write mount", repoClaim.Spec.AccessModes)
	}
	if got := repoClaim.Spec.Resources.Requests["storage"]; got != repoClaimStorage {
		t.Errorf("the repo claim requests storage %q, want %q", got, repoClaimStorage)
	}

	var syncPod Pod
	if err := json.Unmarshal(plan.Apply[1].Stdin, &syncPod); err != nil {
		t.Fatalf("the second apply is not a document: %v", err)
	}
	if syncPod.Kind != "Pod" || syncPod.Metadata.Name != syncPodName(spec) {
		t.Errorf("the second apply is a %s %q, want the sync pod %q", syncPod.Kind, syncPod.Metadata.Name, syncPodName(spec))
	}
	if len(syncPod.Spec.Containers) != 1 || syncPod.Spec.Containers[0].Image != spec.Image {
		t.Errorf("the sync pod's containers are %+v, want one running the Spec's image %q", syncPod.Spec.Containers, spec.Image)
	}

	if argv := plan.Apply[2].Argv; !slices.Contains(argv, "wait") || !slices.Contains(argv, "pod/"+syncPodName(spec)) {
		t.Errorf("the third apply runs %v, want a wait for the sync pod %q", argv, syncPodName(spec))
	}
	cp := plan.Apply[3].Argv
	if !slices.Contains(cp, "cp") || !slices.Contains(cp, spec.RepoDir+"/.") || !slices.Contains(cp, syncPodName(spec)+":"+RepoMount) {
		t.Errorf("the fourth apply runs %v, want the repo contents %q copied into the sync pod at %s", cp, spec.RepoDir+"/.", RepoMount)
	}
	exec := strings.Join(plan.Apply[4].Argv, " ")
	if !strings.Contains(exec, "exec "+syncPodName(spec)) ||
		!strings.Contains(exec, "chown -R 10001:10001 "+RepoMount) ||
		!strings.Contains(exec, "chmod -R a+rX "+RepoMount) {
		t.Errorf("the fifth apply runs %q, want a chown and chmod of the copied tree", exec)
	}
	if argv := plan.Apply[5].Argv; !slices.Contains(argv, "delete") || !slices.Contains(argv, "pod") || !slices.Contains(argv, syncPodName(spec)) {
		t.Errorf("the sixth apply runs %v, want the sync pod %q removed", argv, syncPodName(spec))
	}

	var outClaim Claim
	if err := json.Unmarshal(plan.Apply[6].Stdin, &outClaim); err != nil {
		t.Fatalf("the seventh apply is not a document: %v", err)
	}
	if outClaim.Metadata.Name != OutClaimName(spec) {
		t.Errorf("the seventh apply is the claim %q, want the output claim %q", outClaim.Metadata.Name, OutClaimName(spec))
	}
	if string(plan.Apply[7].Stdin) != string(plan.Job.JSON()) {
		t.Errorf("the last apply does not carry the rendered Job document")
	}
}

// TestSyncPodHasNoServiceAccountToken holds the sync pod to no API access: it
// runs as root to chown the copy, so the default service-account token mounted
// into it would be a root token.
func TestSyncPodHasNoServiceAccountToken(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	plan, err := Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	pod := syncPodFromPlan(t, plan, spec)
	if pod.Spec.AutomountServiceAccountToken == nil {
		t.Fatalf("the sync pod renders no automountServiceAccountToken, so the cluster's default token is mounted into a root pod")
	}
	if *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("the sync pod's automountServiceAccountToken is true; it needs no API access")
	}
}

// TestSyncRefusesWhileASweepJobReadsTheClaim holds the refusal before the repo
// claim is written: the plan's guard lists this rig's sweep Jobs and refuses
// while one is still active, naming it, and the sync pod clears the claim before
// the copy so no object of an earlier run survives it. Both are read from the
// Render plan.
func TestSyncRefusesWhileASweepJobReadsTheClaim(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	plan, err := Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	if len(plan.Guards) != 1 {
		t.Fatalf("the plan renders %d guards, want the one listing this rig's active sweep Jobs", len(plan.Guards))
	}
	guard := plan.Guards[0]
	if !slices.Contains(guard.Step.Argv, "get") || !slices.Contains(guard.Step.Argv, "jobs") {
		t.Errorf("the guard runs %v, want a listing of the rig's sweep Jobs", guard.Step.Argv)
	}
	if !slices.Contains(guard.Step.Argv, "-l") || !slices.Contains(guard.Step.Argv, sweepJobSelector(spec.Rig)) {
		t.Errorf("the guard runs %v, want the selector %q that finds this rig's sweep Jobs", guard.Step.Argv, sweepJobSelector(spec.Rig))
	}
	// The selector finds the Jobs only if the Job carries what it selects.
	labels := plan.Job.Metadata.Labels
	if labels[sweepJobLabel] != sweepJobValue || labels[rigLabelKey] != rigLabelValue(spec.Rig) {
		t.Errorf("the Job's labels are %v, want %q=%q and %q=%q as the guard's selector expects",
			labels, sweepJobLabel, sweepJobValue, rigLabelKey, rigLabelValue(spec.Rig))
	}
	// A running Job has no Complete or Failed condition, so its line is its
	// name alone.
	active := "gt-sweep-gastown-deadbeef-otherrun\n"
	err = guard.Refuse([]byte(active))
	if err == nil || !strings.Contains(err.Error(), "gt-sweep-gastown-deadbeef-otherrun") {
		t.Fatalf("Refuse(%q) = %v, want it refused naming the running Job", active, err)
	}
	for _, ended := range []string{"Complete", "Failed"} {
		line := "gt-sweep-gastown-deadbeef-donerun " + ended + "\n"
		if err := guard.Refuse([]byte(line)); err != nil {
			t.Errorf("Refuse of an ended Job %q = %v, want it to pass", line, err)
		}
	}
	if err := guard.Refuse(nil); err != nil {
		t.Errorf("Refuse of an empty listing = %v, want it to pass", err)
	}
	// A state the guard cannot read is not a Job known to have ended.
	for _, line := range []string{
		"gt-sweep-gastown-deadbeef-otherrun Suspended\n",
		"gt-sweep-gastown-deadbeef-otherrun Complete Failed\n",
	} {
		if err := guard.Refuse([]byte(line)); err == nil {
			t.Errorf("Refuse(%q) passed; a Job whose state is unknown is not a Job known to have ended", line)
		}
	}

	pod := syncPodFromPlan(t, plan, spec)
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("the sync pod renders %d containers, want one", len(pod.Spec.Containers))
	}
	command := pod.Spec.Containers[0].Command
	if len(command) < 3 || command[0] != "sh" || command[1] != "-c" {
		t.Fatalf("the sync pod's command is %v, want a shell script", command)
	}
	if want := "find " + RepoMount + " -mindepth 1 -delete"; !strings.HasPrefix(command[2], want) {
		t.Errorf("the sync pod's script is %q, want it to start by clearing the claim with %q so no stale object survives the copy", command[2], want)
	}
}

// syncPodFromPlan is the sync pod document among the plan's apply steps.
func syncPodFromPlan(t *testing.T, plan Plan, spec Spec) Pod {
	t.Helper()
	for _, step := range plan.Apply {
		var pod Pod
		if step.Stdin == nil || json.Unmarshal(step.Stdin, &pod) != nil {
			continue
		}
		if pod.Kind == "Pod" && pod.Metadata.Name == syncPodName(spec) {
			return pod
		}
	}
	t.Fatalf("the plan applies no sync pod %q", syncPodName(spec))
	return Pod{}
}

// TestK8sPollCopiesTheResultOutOfTheClaim holds the copy-out to its reader: the
// reader pod mounts the output claim read-only at the runner's output directory,
// it ends at its deadline, and the copy lands result.json in the host directory
// the caller reads from.
func TestK8sPollCopiesTheResultOutOfTheClaim(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	plan, err := Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	if plan.Poll == nil {
		t.Fatal("the Kubernetes plan renders no poll; a result left on the claim is never copied out")
	}
	var pod Pod
	if err := json.Unmarshal(plan.Poll.Collect[0].Stdin, &pod); err != nil {
		t.Fatalf("the reader apply is not a document: %v", err)
	}
	if pod.Metadata.Name != readerName(spec) || pod.Kind != "Pod" {
		t.Errorf("the reader is a %s %q, want the pod %q", pod.Kind, pod.Metadata.Name, readerName(spec))
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != readerLifetimeSeconds {
		t.Errorf("the reader's deadline is %v, want %d seconds so it ends whether or not teardown ran", pod.Spec.ActiveDeadlineSeconds, readerLifetimeSeconds)
	}
	if pod.Spec.PriorityClassName != PriorityClass {
		t.Errorf("the reader runs at priority %q, want the CI class %q", pod.Spec.PriorityClassName, PriorityClass)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != readerContainerName {
		t.Fatalf("the reader renders containers %+v, want the one %q container", pod.Spec.Containers, readerContainerName)
	}
	reader := pod.Spec.Containers[0]
	if reader.Resources == nil || reader.Resources.Limits == (ResourceList{}) || reader.Resources.Requests != reader.Resources.Limits {
		t.Errorf("the reader's resources are %+v, want a bounded share with requests equal to limits", reader.Resources)
	}
	if reader.SecurityContext == nil || reader.SecurityContext.AllowPrivilegeEscalation == nil || *reader.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("the reader's security context is %+v, want privilege escalation refused", reader.SecurityContext)
	}
	if !slices.Equal(reader.Command, []string{"sleep", strconv.Itoa(readerLifetimeSeconds)}) {
		t.Errorf("the reader runs %v, want a sleep that holds it for the copy", reader.Command)
	}
	if want := (VolumeMount{Name: outVolume, MountPath: OutputDir, ReadOnly: true}); !slices.Equal(reader.VolumeMounts, []VolumeMount{want}) {
		t.Errorf("the reader's mounts = %+v, want the claim read-only at %s", reader.VolumeMounts, OutputDir)
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].PersistentVolumeClaim == nil ||
		pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != OutClaimName(spec) ||
		!pod.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Errorf("the reader's volumes = %+v, want only the output claim, read-only", pod.Spec.Volumes)
	}

	if len(plan.Poll.Collect) != 3 {
		t.Fatalf("the copy renders %d steps, want the reader's apply, its readiness wait and the copy", len(plan.Poll.Collect))
	}
	dst := filepath.Join(spec.OutDir, ResultFileName)
	if cp := plan.Poll.Collect[2].Argv; !slices.Contains(cp, "cp") || !slices.Contains(cp, readerName(spec)+":"+OutputDir+"/"+ResultFileName) || cp[len(cp)-1] != dst {
		t.Errorf("the copy runs %v, want the result copied from the reader into %q", cp, dst)
	}
	if got := plan.Poll.Status.Argv; !slices.Contains(got, "get") || !slices.Contains(got, "job") || !slices.Contains(got, plan.Job.Metadata.Name) {
		t.Errorf("the status reads %v, want the Job's status", got)
	}
}

// TestK8sPollEndedIsTrueOnlyOnAnEndedJob reads the Job's status output: a run
// with no true condition is still going, and Complete or Failed ends it whichever
// way it went.
func TestK8sPollEndedIsTrueOnlyOnAnEndedJob(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		stdout string
		want   bool
	}{
		{"still running", "", false},
		{"complete", "Complete", true},
		{"failed", "Failed", true},
		{"failed with a trailing newline", "Failed\n", true},
		{"a condition that is not an end", "Suspended", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := jobEnded([]byte(tc.stdout)); got != tc.want {
				t.Errorf("jobEnded(%q) = %v, want %v", tc.stdout, got, tc.want)
			}
		})
	}
}

// containerNamed returns the pod's container called name, init container or
// regular one.
func containerNamed(job *Job, name string) (Container, bool) {
	pod := job.Spec.Template.Spec
	for _, containers := range [][]Container{pod.InitContainers, pod.Containers} {
		for _, container := range containers {
			if container.Name == name {
				return container, true
			}
		}
	}
	return Container{}, false
}

// privilegedContainers returns the names of the pod's privileged containers, in
// the order the pod lists them, its init containers first.
func privilegedContainers(job *Job) []string {
	var names []string
	pod := job.Spec.Template.Spec
	for _, containers := range [][]Container{pod.InitContainers, pod.Containers} {
		for _, container := range containers {
			if container.SecurityContext != nil && container.SecurityContext.Privileged {
				names = append(names, container.Name)
			}
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

// The runner container names the sweep-run binary, so the pod does not start the
// image's default bash and finish with no result. It runs as the unprivileged
// polecat user with no root of its own, and the pod shares one process namespace,
// whose root pause process reaps the orphans the tests kill and is the other
// user's pid 1. Removing the command, the user or the shared namespace turns this
// red, as it does the golden; running the runner as root does too, because it
// would sit beside the privileged dind sidecar in that namespace.
func TestK8sRunnerCommandUserAndSharedProcessNamespace(t *testing.T) {
	t.Parallel()
	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	runner, ok := containerNamed(plan.Job, runnerContainerName)
	if !ok {
		t.Fatalf("the Job renders no %q container", runnerContainerName)
	}
	if !slices.Equal(runner.Command, []string{RunnerCommand}) {
		t.Errorf("runner container command = %q, want [%q]", runner.Command, RunnerCommand)
	}
	if runner.SecurityContext == nil || runner.SecurityContext.RunAsUser == nil || *runner.SecurityContext.RunAsUser != 10001 {
		t.Errorf("runner security context = %+v, want runAsUser 10001 and never root", runner.SecurityContext)
	}
	pod := plan.Job.Spec.Template.Spec
	if pod.ShareProcessNamespace == nil || !*pod.ShareProcessNamespace {
		t.Error("the pod does not share its process namespace: killed grandchildren would stay zombies")
	}
}

// The runner container turns the testcontainers reaper off, as the Docker one does.
func TestK8sRunnerDisablesTheTestcontainersReaper(t *testing.T) {
	t.Parallel()
	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	runner, ok := containerNamed(plan.Job, runnerContainerName)
	if !ok {
		t.Fatalf("the Job renders no %q container", runnerContainerName)
	}
	if !slices.Contains(runner.Env, EnvVar{Name: "TESTCONTAINERS_RYUK_DISABLED", Value: "true"}) {
		t.Errorf("runner env %v does not disable the reaper", runner.Env)
	}
}

// The Kubernetes sidecar has the pod's network and pulls the test images itself,
// so the Job renders no seed volume, claim or init container; adding a seed to one
// backend without the other is a decision this test makes visible.
func TestK8sJobRendersNoSeedStep(t *testing.T) {
	t.Parallel()
	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	if strings.Contains(string(plan.Job.JSON()), SeedVolume) {
		t.Error("the Job mentions the seed volume: the Kubernetes sidecar pulls its images and needs none")
	}
}

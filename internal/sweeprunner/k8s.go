package sweeprunner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/templates"
)

// Kubernetes renders a sweep run as a batch/v1 Job on the town cluster, so the
// run has a share of a machine the daemon's host does not share.
type Kubernetes struct{}

// Compile-time check that Kubernetes satisfies the Backend the daemon renders
// with.
var _ Backend = Kubernetes{}

// Name is the backend's name in the daemon's config and its logs.
func (Kubernetes) Name() string { return "kubernetes" }

const (
	// PriorityClass is the class every sweep Job runs at: the town's CI class,
	// the first workload the cluster sheds under pressure, so a sweep yields to
	// the sandboxes and the control plane (deploy/k8s/priority-classes.yaml).
	PriorityClass = "gt-ci"

	// kubectl is the CLI the plan's teardown step runs.
	kubectl = "kubectl"

	// runnerContainerName and dindContainerName name the pod's two containers.
	runnerContainerName = "runner"
	dindContainerName   = "dind"

	// readerContainerName names the reader pod's one container.
	readerContainerName = "reader"

	// syncContainerName names the sync pod's one container.
	syncContainerName = "sync"

	// repoVolume, outVolume and dindVolume name the pod's volumes.
	repoVolume = "repo"
	outVolume  = "out"
	dindVolume = "dind-data"

	// dindCPU and dindMemory are the sidecar's share, requested and limited like
	// the runner's: an unbounded nested engine competes with the run it serves
	// and with the node's other pods.
	dindCPU    = "2"
	dindMemory = "4Gi"

	// probePeriodSeconds and probeFailureThreshold are the cadence a kubelet
	// checks a container at: the dind sidecar's startup probe, and the sync
	// pod's readiness probe. For the sidecar they bound an engine that never
	// starts listening: the kubelet restarts it within about a minute instead of
	// holding the pod, and its share, until the Job's deadline.
	probePeriodSeconds    int32 = 2
	probeFailureThreshold int32 = 30

	// jobDeadlineSeconds stops a run that hangs on one package, so the share it
	// holds returns to the cluster and the daemon reads the missing result.json
	// as RED. It is a ceiling no healthy sweep approaches, far above the plan's
	// 15-minute loaded target for a whole sweep.
	jobDeadlineSeconds = int64(3 * 60 * 60)

	// jobTTLSeconds removes the finished Job, pods included, an hour after it
	// ends, so a caller that died before its teardown leaks no job. The run's
	// result lives on the output claim, not in the Job.
	jobTTLSeconds = int32(60 * 60)

	// outClaimStorage is the output claim's requested size, for the result and
	// the logs of one sweep.
	outClaimStorage = "5Gi"

	// repoClaimStorage is the repo claim's requested size: the rig's bare
	// repository, with room for the objects a fetch adds.
	repoClaimStorage = "2Gi"

	// syncLifetimeSeconds bounds the sync pod that fills the repo claim: it
	// sleeps this long and the pod is ended at the same mark, so a teardown
	// that never ran leaves no pod running past it.
	syncLifetimeSeconds = 900

	// syncReadyFile is written once the sync pod has cleared the claim, and its
	// readiness probe waits for it: the copy must land on an empty claim, not
	// race a delete that is still removing the previous run's objects.
	syncReadyFile = "/tmp/sync-ready"

	// sweepJobLabel and rigLabelKey label every sweep Job with the backend and
	// rig that made it. The sync guard finds the rig's Jobs by name, not label,
	// because a Job that lost its labels still reads the claim.
	sweepJobLabel = "app.kubernetes.io/name"
	sweepJobValue = "gt-sweep"
	rigLabelKey   = "gt.dev/rig"

	// runLabelKey labels a sync pod with the run that made it, so a launch that
	// lost the create race removes no other run's pod in its teardown.
	runLabelKey = "gt.dev/run"

	// syncJobsJSONPath prints one "<name> <true condition>..." line per Job,
	// for the sync guard to read: a Job whose line names no true Complete or
	// Failed condition is still running.
	syncJobsJSONPath = `jsonpath={range .items[*]}{.metadata.name}{" "}{.status.conditions[?(@.status=="True")].type}{"\n"}{end}`

	// readerLifetimeSeconds bounds the reader pod that copies the result out:
	// it sleeps this long and the pod is ended at the same mark, so a teardown
	// that never ran leaves no pod running past it.
	readerLifetimeSeconds = 900

	// readerCPU and readerMemory are the reader's share, requested and limited
	// alike. It only sleeps and serves a copy, so it takes a sliver of a node.
	readerCPU    = "100m"
	readerMemory = "128Mi"

	// readyTimeout is how long the copy waits for the reader pod to start
	// before the run is judged RED. The reader mounts the output claim, which
	// is ReadWriteOnce: its node can attach the claim only after the Job's pod
	// has ended and the volume has detached, so the wait allows for that.
	readyTimeout = "300s"

	// maxNameLength is the most characters a Kubernetes object's name may hold.
	maxNameLength = 63

	// nameDigestLength is how much of a cut name's digest the name carries:
	// enough that two names a plain cut would run together stay apart.
	nameDigestLength = 12

	// jobNameHead is how much of a name the renderer keeps before the digest
	// when it cuts one for length: the cut leaves room for a dash and the
	// digest.
	jobNameHead = maxNameLength - 1 - nameDigestLength

	// shortSHALength is how much of the commit's sha a name carries: enough to
	// tell two runs apart, short enough to leave the run id its room.
	shortSHALength = 8

	// partDigestLength is how much of a sanitized name part's digest the name
	// carries: enough that two ids a label cannot tell apart stay apart.
	partDigestLength = 8
)

// Job is a sweep run as the document kubectl applies: a batch/v1 Job whose pod
// runs the runner, and the dind sidecar when the Spec asks for one (gt-7zy61).
type Job struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       JobSpec    `json:"spec"`
}

// ObjectMeta is an object's name, which is what a caller reads and removes it
// by, and the labels a caller selects it by.
type ObjectMeta struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

// JobSpec is the Job's single attempt, the deadline it runs under, and the pod
// template it starts.
type JobSpec struct {
	BackoffLimit            *int32      `json:"backoffLimit,omitempty"`
	ActiveDeadlineSeconds   *int64      `json:"activeDeadlineSeconds,omitempty"`
	TTLSecondsAfterFinished *int32      `json:"ttlSecondsAfterFinished,omitempty"`
	Template                PodTemplate `json:"template"`
}

// PodTemplate is the template of the one pod the Job starts.
type PodTemplate struct {
	Spec PodSpec `json:"spec"`
}

// PodSpec is the pod's priority, its restart policy and deadline, whether it
// mounts the default service-account token, the init containers and sidecars
// that start first, the containers that run the work and the volumes they mount.
type PodSpec struct {
	PriorityClassName     string `json:"priorityClassName,omitempty"`
	RestartPolicy         string `json:"restartPolicy"`
	ShareProcessNamespace *bool  `json:"shareProcessNamespace,omitempty"`
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
	// AutomountServiceAccountToken is a pointer so an explicit false is
	// rendered; an absent value leaves the cluster's default, which mounts the
	// namespace's service-account token into every container.
	AutomountServiceAccountToken *bool       `json:"automountServiceAccountToken,omitempty"`
	InitContainers               []Container `json:"initContainers,omitempty"`
	Containers                   []Container `json:"containers"`
	Volumes                      []Volume    `json:"volumes"`
}

// Pod is a single pod the caller applies on its own, outside any Job.
type Pod struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       PodSpec    `json:"spec"`
}

// Claim is the run's output claim as the document kubectl applies: the
// PersistentVolumeClaim the runner writes result.json and its logs to.
type Claim struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       ClaimSpec  `json:"spec"`
}

// ClaimSpec is the claim's access mode and the storage it asks for.
type ClaimSpec struct {
	AccessModes []string       `json:"accessModes"`
	Resources   ClaimResources `json:"resources"`
}

// ClaimResources is the claim's requested quantities, by resource name.
type ClaimResources struct {
	Requests map[string]string `json:"requests"`
}

// Container is one container of the pod.
type Container struct {
	Name            string           `json:"name"`
	Image           string           `json:"image"`
	Command         []string         `json:"command,omitempty"`
	Args            []string         `json:"args,omitempty"`
	RestartPolicy   string           `json:"restartPolicy,omitempty"`
	Env             []EnvVar         `json:"env,omitempty"`
	StartupProbe    *Probe           `json:"startupProbe,omitempty"`
	ReadinessProbe  *Probe           `json:"readinessProbe,omitempty"`
	Resources       *Resources       `json:"resources,omitempty"`
	SecurityContext *SecurityContext `json:"securityContext,omitempty"`
	VolumeMounts    []VolumeMount    `json:"volumeMounts,omitempty"`
}

// Probe is a container's health check: the action that reports it healthy and
// the cadence the kubelet runs it at.
type Probe struct {
	Exec             *ExecAction `json:"exec,omitempty"`
	PeriodSeconds    int32       `json:"periodSeconds,omitempty"`
	FailureThreshold int32       `json:"failureThreshold,omitempty"`
}

// ExecAction is a probe that runs a command inside the container, in the
// container's own network namespace.
type ExecAction struct {
	Command []string `json:"command"`
}

// EnvVar is one environment variable.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Resources is a container's share, given identically as requests and limits so
// the scheduler promises it rather than allowing it as a burst.
type Resources struct {
	Requests ResourceList `json:"requests"`
	Limits   ResourceList `json:"limits"`
}

// ResourceList is the CPU and memory quantities of a Resources.
type ResourceList struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// SecurityContext is the capability a container needs: the dind sidecar runs its
// own engine, and no other container of the pod holds one. AllowPrivilegeEscalation
// is a pointer so an explicit false is rendered; an absent value leaves the
// cluster's default, which allows escalation.
type SecurityContext struct {
	Privileged               bool   `json:"privileged,omitempty"`
	RunAsUser                *int64 `json:"runAsUser,omitempty"`
	AllowPrivilegeEscalation *bool  `json:"allowPrivilegeEscalation,omitempty"`
}

// VolumeMount is one mount of a container.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// Volume is one volume of the pod, named by exactly one source: a claim, or
// scratch space that lives as long as the pod.
type Volume struct {
	Name                  string                `json:"name"`
	PersistentVolumeClaim *ClaimRef             `json:"persistentVolumeClaim,omitempty"`
	EmptyDir              *EmptyDirVolumeSource `json:"emptyDir,omitempty"`
}

// ClaimRef names the claim a volume mounts, and whether the mount is read-only.
type ClaimRef struct {
	ClaimName string `json:"claimName"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// EmptyDirVolumeSource is scratch space that lives as long as the pod.
type EmptyDirVolumeSource struct{}

// Render returns the run as a Job and the steps that remove it, or the Validate
// error that refuses the Spec. The guard refuses while another sweep Job of the
// rig is still active, because every run of the rig reads the one repo claim.
// Apply then creates the repo claim, fills it through a short-lived root sync pod
// (cleared, copied into, chowned to the runner, removed), and only then creates
// the output claim and the Job. The teardown removes the sync pod, the Job, the
// reader pod and the output claim, in that order, and never the repo claim,
// which every run reuses. Poll reports when the run has ended and copies its
// result and its per-unit logs out of the output claim onto the host; the
// caller reads the host copy, and runs the teardown after that read.
func (Kubernetes) Render(spec Spec) (Plan, error) {
	if err := spec.Validate(); err != nil {
		return Plan{}, err
	}
	job := renderJob(spec)
	repoClaim := renderClaim(RepoClaimName(spec.Rig), repoClaimStorage)
	claim := renderClaim(OutClaimName(spec), outClaimStorage)
	reader := renderReader(spec)
	sync := renderSyncPod(spec)
	return Plan{
		Job: job,
		Guards: []Guard{{
			// The repo claim is shared by every run of the rig, and a run's Job
			// mounts it read-only while the sweep runs: rewriting it under an
			// active run replaces the repository that run is still reading. The
			// listing takes every Job, because an unlabelled run still reads the
			// claim; the names decide which Jobs are this rig's, by the structure
			// the Job renderer gives them.
			Step:   Step{Argv: kubectlArgv(spec, "get", "jobs", "-o", syncJobsJSONPath)},
			Refuse: refuseActiveSweepJobs(spec),
		}},
		Apply: []Step{
			// The repo claim is created before the sync pod mounts it, and stays
			// for the next run: only its contents change per run.
			{Argv: kubectlArgv(spec, "apply", "-f", "-"), Stdin: repoClaim.JSON()},
			// Created, not applied: there is one sync pod per rig, so a second
			// sync started while this one runs fails AlreadyExists instead of
			// clearing the claim under it.
			{Argv: kubectlArgv(spec, "create", "-f", "-"), Stdin: sync.JSON()},
			// The pod clears the claim before it reports ready, so the copy starts
			// on an empty mount and no object of the previous run survives.
			{Argv: kubectlArgv(spec, "wait", "--for=condition=Ready", "pod/"+sync.Metadata.Name, "--timeout="+readyTimeout)},
			// kubectl cp restores the ids the host user's files carried, which
			// the runner cannot use, so the exec that follows gives the tree to
			// the runner's user and makes it readable.
			{Argv: kubectlArgv(spec, "cp", spec.RepoDir+"/.", sync.Metadata.Name+":"+RepoMount)},
			{Argv: kubectlArgv(spec, "exec", sync.Metadata.Name, "--", "sh", "-c", syncChownCommand())},
			{Argv: kubectlArgv(spec, "delete", "pod", sync.Metadata.Name, "--ignore-not-found")},
			// The claim the Job writes its result to goes before the Job, which
			// has nothing to write to otherwise.
			{Argv: kubectlArgv(spec, "apply", "-f", "-"), Stdin: claim.JSON()},
			{Argv: kubectlArgv(spec, "apply", "-f", "-"), Stdin: job.JSON()},
		},
		Steps: []Step{
			// The sync pod mounts the repo claim, so a teardown after a failed
			// sync removes it too; the claim itself is never removed. The run
			// label selects only a pod this run created: a launch that lost the
			// create race shares the pod's name and made nothing.
			{Argv: kubectlArgv(spec, "delete", "pod", "-l", runLabelSelector(spec), "--ignore-not-found"), Teardown: true},
			{Argv: kubectlArgv(spec, "delete", "job", job.Metadata.Name, "--ignore-not-found"), Teardown: true},
			// The reader pod mounts the claim, so it goes before the claim does.
			{Argv: kubectlArgv(spec, "delete", "pod", reader.Metadata.Name, "--ignore-not-found"), Teardown: true},
			// The run's output claim is the caller's to create and its teardown
			// removes it: a Job-only teardown leaves every run's claim behind
			// (gt-7zy61).
			{Argv: kubectlArgv(spec, "delete", "persistentvolumeclaim", claim.Metadata.Name, "--ignore-not-found"), Teardown: true},
		},
		Poll: &Poll{
			Status: Step{Argv: kubectlArgv(spec, "get", "job", job.Metadata.Name, "-o", `jsonpath={.status.conditions[?(@.status=="True")].type}`)},
			Ended:  jobEnded,
			Collect: []Step{
				{Argv: kubectlArgv(spec, "apply", "-f", "-"), Stdin: reader.JSON()},
				{Argv: kubectlArgv(spec, "wait", "--for=condition=Ready", "pod/"+reader.Metadata.Name, "--timeout="+readyTimeout)},
				{Argv: kubectlArgv(spec, "cp", reader.Metadata.Name+":"+OutputDir+"/"+ResultFileName, filepath.Join(spec.OutDir, ResultFileName))},
				// The per-unit logs go beside the result before the teardown
				// removes the claim, so a RED summary's log path resolves on
				// the host. A run that wrote none leaves the source absent,
				// which is not a failure (gt-4lj2q).
				{Argv: kubectlArgv(spec, "cp", reader.Metadata.Name+":"+OutputDir+"/"+LogsDirName, filepath.Join(spec.OutDir, LogsDirName)), MissingSourcePath: OutputDir + "/" + LogsDirName},
			},
		},
	}, nil
}

// kubectlArgv is a kubectl command against the Spec's context.
func kubectlArgv(spec Spec, args ...string) []string {
	return append([]string{kubectl, "--context", spec.Context}, args...)
}

// jobEnded reports from a Job's condition types whether it has ended: a Job
// with no true condition is still running, and one with Complete or Failed true
// has ended, whichever way it went.
func jobEnded(stdout []byte) bool {
	return hasEndedCondition(strings.Fields(string(stdout)))
}

// manifestJSON is a document as kubectl applies it.
func manifestJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return buf.Bytes()
}

// JSON is the Job as the document kubectl applies.
func (j *Job) JSON() []byte { return manifestJSON(j) }

// JSON is the output claim as the document kubectl applies.
func (c *Claim) JSON() []byte { return manifestJSON(c) }

// JSON is the pod as the document kubectl applies.
func (p *Pod) JSON() []byte { return manifestJSON(p) }

// renderClaim is a claim the plan applies: ReadWriteOnce, so one node mounts it
// at a time, asking for the given storage.
func renderClaim(name, storage string) *Claim {
	return &Claim{
		APIVersion: "v1",
		Kind:       "PersistentVolumeClaim",
		Metadata:   ObjectMeta{Name: name},
		Spec: ClaimSpec{
			AccessModes: []string{"ReadWriteOnce"},
			Resources:   ClaimResources{Requests: map[string]string{"storage": storage}},
		},
	}
}

// renderReader is the pod that copies the result out of the claim once the Job
// has ended: it mounts the claim read-only and sleeps, so kubectl can copy from
// it, and it ends at the deadline whether or not a teardown removed it.
func renderReader(spec Spec) *Pod {
	share := ResourceList{CPU: readerCPU, Memory: readerMemory}
	return &Pod{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata:   ObjectMeta{Name: readerName(spec)},
		Spec: PodSpec{
			PriorityClassName:     PriorityClass,
			RestartPolicy:         "Never",
			ActiveDeadlineSeconds: int64Ptr(readerLifetimeSeconds),
			Containers: []Container{{
				Name:            readerContainerName,
				Image:           spec.Image,
				Command:         []string{"sleep", strconv.Itoa(readerLifetimeSeconds)},
				Resources:       &Resources{Requests: share, Limits: share},
				SecurityContext: &SecurityContext{AllowPrivilegeEscalation: boolPtr(false)},
				VolumeMounts:    []VolumeMount{{Name: outVolume, MountPath: OutputDir, ReadOnly: true}},
			}},
			Volumes: []Volume{{Name: outVolume, PersistentVolumeClaim: &ClaimRef{ClaimName: OutClaimName(spec), ReadOnly: true}}},
		},
	}
}

// readerName is the reader pod's name, which the run id makes unique to the
// launch.
func readerName(spec Spec) string {
	return k8sName("gt-sweep-read", spec.Rig, shortSHA(spec.SHA), spec.RunID)
}

// renderSyncPod is the short-lived pod that refills the repo claim before the
// Job starts: it mounts the claim read-write as root, clears it, and holds its
// mount while kubectl copies the host bare repository into it and chowns it to
// the runner. It ends at its deadline whether or not a teardown removed it.
func renderSyncPod(spec Spec) *Pod {
	share := ResourceList{CPU: readerCPU, Memory: readerMemory}
	return &Pod{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata:   ObjectMeta{Name: syncPodName(spec), Labels: map[string]string{runLabelKey: runLabelValue(spec.RunID)}},
		Spec: PodSpec{
			PriorityClassName:     PriorityClass,
			RestartPolicy:         "Never",
			ActiveDeadlineSeconds: int64Ptr(syncLifetimeSeconds),
			// It runs as root to chown what the copy leaves and needs no API
			// access, so it must not carry the namespace's service-account
			// token, which would be a root token.
			AutomountServiceAccountToken: boolPtr(false),
			Containers: []Container{{
				Name:    syncContainerName,
				Image:   spec.Image,
				Command: []string{"sh", "-c", syncCommand()},
				// Cleared before the pod reports ready, so the copy starts on an
				// empty mount.
				ReadinessProbe: &Probe{
					Exec:             &ExecAction{Command: []string{"sh", "-c", "test -f " + syncReadyFile}},
					PeriodSeconds:    probePeriodSeconds,
					FailureThreshold: probeFailureThreshold,
				},
				Resources:       &Resources{Requests: share, Limits: share},
				SecurityContext: &SecurityContext{RunAsUser: int64Ptr(0), AllowPrivilegeEscalation: boolPtr(false)},
				VolumeMounts:    []VolumeMount{{Name: repoVolume, MountPath: RepoMount}},
			}},
			// Read-write: the pod clears the claim and the copy fills it.
			Volumes: []Volume{{Name: repoVolume, PersistentVolumeClaim: &ClaimRef{ClaimName: RepoClaimName(spec.Rig)}}},
		},
	}
}

// syncCommand is the sync pod's command: clear the claim, mark it ready for the
// copy, and hold the mount until the teardown removes the pod or its deadline
// ends it. The marker is what the readiness probe waits on, so a delete that
// failed leaves the pod not ready and the stage RED rather than copying into a
// claim still holding the previous run's objects.
func syncCommand() string {
	return "find " + RepoMount + " -mindepth 1 -delete && touch " + syncReadyFile +
		" && sleep " + strconv.Itoa(syncLifetimeSeconds)
}

// syncChownCommand gives the copied tree to the runner's user and makes it
// readable, because kubectl cp restores the ids the host user's files carried.
func syncChownCommand() string {
	owner := strconv.FormatInt(runnerUser, 10) + ":" + strconv.FormatInt(runnerUser, 10)
	return "chown -R " + owner + " " + RepoMount + " && chmod -R a+rX " + RepoMount
}

// syncPodName is the sync pod's name, which one rig shares: a second sync
// started while it exists fails AlreadyExists, so two runs never clear and copy
// into the claim at once.
func syncPodName(spec Spec) string {
	return k8sName("gt-sweep-sync", spec.Rig)
}

// renderJob is the Job for a Spec that has passed Validate.
func renderJob(spec Spec) *Job {
	containers := []Container{runnerContainer(spec)}
	volumes := []Volume{
		// The run reads the rig's bare repository and never writes it, so the
		// claim the deploy fills is mounted read-only.
		{Name: repoVolume, PersistentVolumeClaim: &ClaimRef{ClaimName: RepoClaimName(spec.Rig), ReadOnly: true}},
		// The runner leaves result.json and its logs here, and the caller reads
		// them from the same claim after the Job ends.
		{Name: outVolume, PersistentVolumeClaim: &ClaimRef{ClaimName: OutClaimName(spec)}},
	}
	var initContainers []Container
	if spec.Dind {
		// The engine is a native sidecar, not a second regular container: the
		// Job ends when the runner exits, and the kubelet then stops the sidecar
		// too, so the run gives its share back instead of running to the
		// deadline (gt-etbor).
		initContainers = append(initContainers, dindContainer())
		volumes = append(volumes, Volume{Name: dindVolume, EmptyDir: &EmptyDirVolumeSource{}})
	}
	return &Job{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata:   ObjectMeta{Name: JobName(spec), Labels: jobLabels(spec)},
		Spec: JobSpec{
			// A failed run reports itself once: the result the runner did not
			// write is what the daemon reads as RED, and a retried pod would
			// repeat the same failure until the Job's own limit ran out.
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   int64Ptr(jobDeadlineSeconds),
			TTLSecondsAfterFinished: int32Ptr(jobTTLSeconds),
			Template: PodTemplate{Spec: PodSpec{
				PriorityClassName: PriorityClass,
				RestartPolicy:     "Never",
				// The pod's pause process is pid 1 of the shared namespace and
				// reaps the orphans the tests kill; without it a killed
				// grandchild stays a zombie and still answers kill(pid, 0).
				ShareProcessNamespace: boolPtr(true),
				InitContainers:        initContainers,
				Containers:            containers,
				Volumes:               volumes,
			}},
		},
	}
}

// runnerContainer is the container the sweep runs in.
func runnerContainer(spec Spec) Container {
	env := make([]EnvVar, 0, 5)
	if spec.Dind {
		env = append(env, EnvVar{Name: "DOCKER_HOST", Value: "tcp://" + dindAddress + ":" + dindPort})
	}
	env = append(env,
		// The unprivileged user's own home belongs to another user.
		EnvVar{Name: "HOME", Value: "/tmp"},
		EnvVar{Name: ryukDisabledName, Value: ryukDisabledValue},
		EnvVar{Name: EnvRig, Value: spec.Rig},
		EnvVar{Name: EnvSHA, Value: spec.SHA},
		EnvVar{Name: EnvTiers, Value: strings.Join(spec.TiersOrDefault(), ",")},
		EnvVar{Name: EnvJobs, Value: strconv.Itoa(spec.Jobs)},
	)
	// Requests equal limits, so the scheduler promises the run its whole share
	// instead of letting it burst into a neighbour's (design 2026-10-30).
	share := ResourceList{CPU: strconv.Itoa(spec.CPUs), Memory: strconv.Itoa(spec.MemoryGB) + "Gi"}
	return Container{
		Name:    runnerContainerName,
		Image:   spec.Image,
		Command: []string{RunnerCommand},
		// The unprivileged polecat user from the start: the pod's shared process
		// namespace has a root pause process as pid 1, so a test that signals
		// pid 1 still sees another user's process, and the runner holds no root
		// next to the privileged dind sidecar it shares that namespace with.
		SecurityContext: &SecurityContext{RunAsUser: int64Ptr(runnerUser)},
		Env:             env,
		Resources:       &Resources{Requests: share, Limits: share},
		VolumeMounts: []VolumeMount{
			{Name: repoVolume, MountPath: RepoMount, ReadOnly: true},
			{Name: outVolume, MountPath: OutputDir},
		},
	}
}

// dindContainer is the sidecar the packages that need a Docker API reach: the
// pod's only privileged container, bound to the pod's loopback so no other pod
// in the cluster can drive the engine. Unlike the Docker backend's sidecar, which
// sits on an internal network with no route out and so starts from a seed volume,
// this one has the pod's network and pulls the images the tests start itself, so
// the Job renders no seed step (gt-aw5jn).
//
// It is rendered as a native sidecar: restartPolicy Always on an init container
// makes the kubelet start it before the runner and stop it when the runner
// exits, so the Job completes and the pod releases its share (gt-etbor).
func dindContainer() Container {
	share := ResourceList{CPU: dindCPU, Memory: dindMemory}
	return Container{
		Name:  dindContainerName,
		Image: templates.SweepDindImage,
		Args: []string{
			// dockerd named as the command, so the image's entrypoint adds no
			// host of its own on 0.0.0.0:2375 beside the loopback one below.
			"dockerd",
			// Without an explicit host dockerd binds 0.0.0.0, which is the pod IP
			// and a privileged Docker API for every pod on the cluster network.
			"--host=tcp://" + dindAddress + ":" + dindPort,
			"--tls=false",
		},
		RestartPolicy: "Always",
		Env:           []EnvVar{{Name: "DOCKER_TLS_CERTDIR", Value: ""}},
		// The runner starts only after the engine answers, and the probe must run
		// inside this container: the kubelet dials a tcpSocket or httpGet probe
		// from the node's network namespace, where the engine's loopback is not
		// listening, while an exec probe runs beside dockerd and reaches it.
		StartupProbe: &Probe{
			Exec:             &ExecAction{Command: []string{"docker", "-H", "tcp://" + dindAddress + ":" + dindPort, "info"}},
			PeriodSeconds:    probePeriodSeconds,
			FailureThreshold: probeFailureThreshold,
		},
		Resources:       &Resources{Requests: share, Limits: share},
		SecurityContext: &SecurityContext{Privileged: true},
		VolumeMounts:    []VolumeMount{{Name: dindVolume, MountPath: dindDataPath}},
	}
}

// JobName is the Job's name, which the run id makes unique to the launch.
func JobName(spec Spec) string {
	return k8sName("gt-sweep", spec.Rig, shortSHA(spec.SHA), spec.RunID)
}

// RepoClaimName is the claim holding the rig's bare repository, which its runs
// mount read-only.
func RepoClaimName(rig string) string { return k8sName("gt-sweep-repo", rig) }

// jobLabels mark a sweep Job with the backend and rig that made it.
func jobLabels(spec Spec) map[string]string {
	return map[string]string{sweepJobLabel: sweepJobValue, rigLabelKey: rigLabelValue(spec.Rig)}
}

// sweepJobStem is the part of a sweep Job's name every run of the rig shares:
// the name the renderer makes for spec, without the run-specific short sha and
// run id. A name the renderer cut for length carries a digest of the whole name
// in their place, so there is none to cut off and the stem is the rig's own
// head of the name.
func sweepJobStem(spec Spec) string {
	name := JobName(spec)
	tail := "-" + k8sPart(shortSHA(spec.SHA)) + "-" + k8sPart(spec.RunID)
	if stem, ok := strings.CutSuffix(name, tail); ok {
		return stem
	}
	return k8sPart("gt-sweep") + "-" + k8sPart(spec.Rig)
}

// sweepJobSelector returns the test the guard applies to a listed Job's name: it
// reports whether the name is one the renderer makes for spec's rig. The
// renderer names a Job "<stem>-<short sha>-<run id>", so a name belongs when
// the segment after the stem is a run's short sha. A rig whose name extends
// this one's — "gastown-x" beside "gastown" — contributes "x" there and is not
// counted, whichever of the two is being run. A rig whose own name continues
// with eight hex digits reads as this rig's short sha: the names cannot tell
// those apart, and counting both refuses a run rather than let it rewrite the
// claim under a sweep.
//
// A name the renderer cut for length keeps that part only as far as the cut,
// so the test reads the kept prefix instead.
func sweepJobSelector(spec Spec) func(string) bool {
	stem := sweepJobStem(spec)
	// A stem this long fills the head the renderer keeps before the short sha
	// starts, leaving no run part to read.
	if len(stem) >= jobNameHead-1 {
		head := stem
		if len(head) > jobNameHead {
			head = strings.TrimRight(head[:jobNameHead], "-")
		}
		// The kept head is all a cut name of this rig can share; a longer rig
		// that shares it is refused too, which only leaves the claim alone.
		return func(name string) bool { return strings.HasPrefix(name, head+"-") }
	}
	return func(name string) bool {
		tail, ok := strings.CutPrefix(name, stem+"-")
		return ok && isJobRunTail(tail)
	}
}

// isJobRunTail reports whether tail is what the renderer appends after a rig's
// stem: a run's short sha and run id, or the part of them a cut for length kept
// ahead of the digest the cut appends.
func isJobRunTail(tail string) bool {
	if len(tail) > nameDigestLength && tail[len(tail)-nameDigestLength-1] == '-' &&
		isHex(tail[len(tail)-nameDigestLength:]) &&
		isRunTailPrefix(tail[:len(tail)-nameDigestLength-1]) {
		return true
	}
	return isRunTailPrefix(tail)
}

// isRunTailPrefix reports whether s is a prefix of a run tail, the run's short
// sha and run id the renderer appends after the rig's stem.
func isRunTailPrefix(s string) bool {
	sha, rest, ok := strings.Cut(s, "-")
	if !ok {
		return len(sha) <= shortSHALength && isHex(sha)
	}
	return len(sha) == shortSHALength && isHex(sha) && isNamePartChars(rest)
}

// isHex reports whether s holds only lower-case hex digits, as the short sha a
// name carries and the digest a cut appends do.
func isHex(s string) bool {
	for _, r := range s {
		if !isHexDigit(r) {
			return false
		}
	}
	return true
}

// isHexDigit reports whether r is one lower-case hex digit.
func isHexDigit(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f'
}

// isNamePartChars reports whether s holds only the characters one part of a
// rendered name carries: lower-case letters, digits and dashes.
func isNamePartChars(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// runLabelSelector selects the sync pod a run created.
func runLabelSelector(spec Spec) string {
	return runLabelKey + "=" + runLabelValue(spec.RunID)
}

// runLabelValue is the run's value for the sync pod's run label: the sanitized
// run id, cut to the label rule, so one run's teardown never selects another
// run's pod.
func runLabelValue(runID string) string { return labelValue(runID) }

// rigLabelValue is the rig's value for the Job's rig label, so one rig's runs
// are never mistaken for another's.
func rigLabelValue(rig string) string { return labelValue(rig) }

// labelValue cuts v to a label value of at most 63 characters, with a digest of
// the original when the cut loses part of it.
func labelValue(v string) string {
	part := k8sPart(v)
	if len(part) <= maxNameLength {
		return part
	}
	digest := partDigest(v)
	return strings.TrimRight(part[:maxNameLength-1-len(digest)], "-") + "-" + digest
}

// refuseActiveSweepJobs reads the guard's listing of every Job — one
// "<name> <true condition>..." line each — and refuses while a Job the renderer
// made for spec's rig has not ended: that Job may still be reading the repo
// claim this run is about to rewrite. A name belongs to the rig when the
// renderer's structure says so, so a rig whose name extends this one's is not
// counted by the shorter one. A Job has ended when any true condition is
// Complete or Failed, whatever other conditions the cluster lists beside it and
// in whatever order; a line naming no such condition refuses, because a Job
// whose state is unknown is not a Job known to have ended.
func refuseActiveSweepJobs(spec Spec) func(stdout []byte) error {
	selector := sweepJobSelector(spec)
	return func(stdout []byte) error {
		for _, line := range strings.Split(string(stdout), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || !selector(fields[0]) {
				continue
			}
			if hasEndedCondition(fields[1:]) {
				continue
			}
			return fmt.Errorf("sweep job %s has not ended and may still be reading the repo claim", fields[0])
		}
		return nil
	}
}

// hasEndedCondition reports whether any of a Job's true condition types is
// Complete or Failed.
func hasEndedCondition(conditions []string) bool {
	for _, condition := range conditions {
		if condition == "Complete" || condition == "Failed" {
			return true
		}
	}
	return false
}

// OutClaimName is the claim this run writes its result and logs to: the caller
// creates it before applying the Job and reads the run from it.
func OutClaimName(spec Spec) string {
	return k8sName("gt-sweep-out", spec.Rig, shortSHA(spec.SHA), spec.RunID)
}

// k8sName joins parts into a DNS-1123 label of at most maxNameLength characters:
// each part is lowercased, and every character a label cannot hold becomes a
// dash. A name too long is cut and given a digest of what it was cut from, so
// two runs a plain cut would run together keep their own names.
func k8sName(parts ...string) string {
	cleaned := make([]string, len(parts))
	for i, part := range parts {
		cleaned[i] = k8sPart(part)
	}
	name := strings.Join(cleaned, "-")
	if len(name) <= maxNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	digest := hex.EncodeToString(sum[:])[:nameDigestLength]
	head := strings.TrimRight(name[:maxNameLength-1-len(digest)], "-")
	return head + "-" + digest
}

// k8sPart is one part of a name: lowercased, with every character a label
// cannot hold turned into a dash, and with the dashes a label cannot start or
// end with trimmed off. A part that is not its own label as written carries a
// digest of the value it was made from, so two ids a label cannot tell apart
// once lowercased and dashed ("a.b" and "a-b", "Run" and "run") keep their own
// names and neither run's teardown can remove the other's objects (gt-7zy61).
func k8sPart(v string) string {
	lowered := strings.ToLower(v)
	var b strings.Builder
	for _, r := range lowered {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	part := strings.Trim(b.String(), "-")
	if part == "" {
		return "x-" + partDigest(v)
	}
	if part == v {
		return part
	}
	return part + "-" + partDigest(v)
}

// partDigest is the short digest a name part that sanitizing changed carries.
func partDigest(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:partDigestLength]
}

// shortSHA is the sha's first shortSHALength characters, or all of a sha too
// short to cut: a name built from it must not panic on a Spec that has not been
// validated yet.
func shortSHA(sha string) string {
	if len(sha) < shortSHALength {
		return sha
	}
	return sha[:shortSHALength]
}

// int32Ptr returns a pointer to v, for the fields a Job's absent value differs
// from its zero one.
func int32Ptr(v int32) *int32 { return &v }

// int64Ptr returns a pointer to v, for the fields a Job's absent value differs
// from its zero one.
func int64Ptr(v int64) *int64 { return &v }

// boolPtr returns a pointer to v, so a false value is rendered rather than
// omitted.
func boolPtr(v bool) *bool { return &v }

package sweeprunner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	// repoVolume, outVolume and dindVolume name the pod's volumes.
	repoVolume = "repo"
	outVolume  = "out"
	dindVolume = "dind-data"

	// dindCPU and dindMemory are the sidecar's share, requested and limited like
	// the runner's: an unbounded nested engine competes with the run it serves
	// and with the node's other pods.
	dindCPU    = "2"
	dindMemory = "4Gi"

	// dindProbePort is dindPort as the integer a tcpSocket probe takes; the two
	// are held equal by TestRenderJobDindIsANativeSidecar.
	dindProbePort int32 = 2375

	// dindProbePeriodSeconds and dindProbeFailureThreshold bound a sidecar whose
	// engine never starts listening: the kubelet restarts it within about a
	// minute instead of holding the pod, and its share, until the Job's deadline.
	dindProbePeriodSeconds    int32 = 2
	dindProbeFailureThreshold int32 = 30

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

// ObjectMeta is the Job's name, which is what a caller reads and removes it by.
type ObjectMeta struct {
	Name string `json:"name"`
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

// PodSpec is the pod's priority, its restart policy, its deadline, the init
// containers and sidecars that start first, the containers that run the work and
// the volumes they mount.
type PodSpec struct {
	PriorityClassName     string      `json:"priorityClassName,omitempty"`
	RestartPolicy         string      `json:"restartPolicy"`
	ShareProcessNamespace *bool       `json:"shareProcessNamespace,omitempty"`
	ActiveDeadlineSeconds *int64      `json:"activeDeadlineSeconds,omitempty"`
	InitContainers        []Container `json:"initContainers,omitempty"`
	Containers            []Container `json:"containers"`
	Volumes               []Volume    `json:"volumes"`
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
	Resources       *Resources       `json:"resources,omitempty"`
	SecurityContext *SecurityContext `json:"securityContext,omitempty"`
	VolumeMounts    []VolumeMount    `json:"volumeMounts,omitempty"`
}

// Probe is a container's health check: the action that reports it healthy and
// the cadence the kubelet runs it at.
type Probe struct {
	TCPSocket        *TCPSocketAction `json:"tcpSocket,omitempty"`
	PeriodSeconds    int32            `json:"periodSeconds,omitempty"`
	FailureThreshold int32            `json:"failureThreshold,omitempty"`
}

// TCPSocketAction is a probe that succeeds when the address accepts a TCP
// connection. An empty host is the pod's IP, not the pod's loopback.
type TCPSocketAction struct {
	Host string `json:"host,omitempty"`
	Port int32  `json:"port"`
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
// error that refuses the Spec. Apply creates the output claim, then the Job. The
// teardown removes the Job, the reader pod and the claim, in that order, so the
// claim is not removed while a pod still mounts it. Poll reports when the run
// has ended and copies its result out of the claim onto the host; the caller
// reads the host copy, and runs the teardown after that read.
func (Kubernetes) Render(spec Spec) (Plan, error) {
	if err := spec.Validate(); err != nil {
		return Plan{}, err
	}
	job := renderJob(spec)
	claim := renderOutClaim(spec)
	reader := renderReader(spec)
	return Plan{
		Job: job,
		Apply: []Step{
			// The claim goes first: a Job applied before its claim has nothing to
			// write its result to.
			{Argv: kubectlArgv(spec, "apply", "-f", "-"), Stdin: claim.JSON()},
			{Argv: kubectlArgv(spec, "apply", "-f", "-"), Stdin: job.JSON()},
		},
		Steps: []Step{
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
	for _, field := range strings.Fields(string(stdout)) {
		if field == "Complete" || field == "Failed" {
			return true
		}
	}
	return false
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

// renderOutClaim is the claim this run writes its result and logs to. It is
// ReadWriteOnce, so one node mounts it at a time: the Job writes it, and the
// reader mounts it only after the Job has ended.
func renderOutClaim(spec Spec) *Claim {
	return &Claim{
		APIVersion: "v1",
		Kind:       "PersistentVolumeClaim",
		Metadata:   ObjectMeta{Name: OutClaimName(spec)},
		Spec: ClaimSpec{
			AccessModes: []string{"ReadWriteOnce"},
			Resources:   ClaimResources{Requests: map[string]string{"storage": outClaimStorage}},
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
		Metadata:   ObjectMeta{Name: JobName(spec)},
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
		// The runner starts only after the engine accepts connections on the
		// port it is handed; the host is the sidecar's own loopback, since an
		// empty host would probe the pod IP where nothing listens.
		StartupProbe: &Probe{
			TCPSocket:        &TCPSocketAction{Host: dindAddress, Port: dindProbePort},
			PeriodSeconds:    dindProbePeriodSeconds,
			FailureThreshold: dindProbeFailureThreshold,
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
	digest := hex.EncodeToString(sum[:])[:12]
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

// shortSHA is the sha's first eight characters, or all of a sha too short to
// cut: a name built from it must not panic on a Spec that has not been
// validated yet.
func shortSHA(sha string) string {
	if len(sha) < 8 {
		return sha
	}
	return sha[:8]
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

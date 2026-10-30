package sweeprunner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	// repoVolume, outVolume and dindVolume name the pod's volumes.
	repoVolume = "repo"
	outVolume  = "out"
	dindVolume = "dind-data"

	// dindAddress is where the runner reaches the sidecar: the pod's own network
	// namespace, so nothing outside the pod reaches the engine. The sidecar binds
	// this address too; a bind on 0.0.0.0 would answer at the pod IP, where every
	// other pod in the cluster could drive the privileged engine (gt-7zy61).
	dindAddress = "127.0.0.1"

	// dindCPU and dindMemory are the sidecar's share, requested and limited like
	// the runner's: an unbounded nested engine competes with the run it serves
	// and with the node's other pods.
	dindCPU    = "2"
	dindMemory = "4Gi"

	// jobDeadlineSeconds stops a run that hangs on one package, so the share it
	// holds returns to the cluster and the daemon reads the missing result.json
	// as RED. It is a ceiling no healthy sweep approaches, far above the plan's
	// 15-minute loaded target for a whole sweep.
	jobDeadlineSeconds = int64(3 * 60 * 60)

	// jobTTLSeconds removes the finished Job, pods included, an hour after it
	// ends, so a caller that died before its teardown leaks no job. The run's
	// result lives on the output claim, not in the Job.
	jobTTLSeconds = int32(60 * 60)

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

// PodSpec is the pod's priority, its restart policy, its containers and the
// volumes they mount.
type PodSpec struct {
	PriorityClassName string      `json:"priorityClassName,omitempty"`
	RestartPolicy     string      `json:"restartPolicy"`
	Containers        []Container `json:"containers"`
	Volumes           []Volume    `json:"volumes"`
}

// Container is one container of the pod.
type Container struct {
	Name            string           `json:"name"`
	Image           string           `json:"image"`
	Args            []string         `json:"args,omitempty"`
	Env             []EnvVar         `json:"env,omitempty"`
	Resources       *Resources       `json:"resources,omitempty"`
	SecurityContext *SecurityContext `json:"securityContext,omitempty"`
	VolumeMounts    []VolumeMount    `json:"volumeMounts,omitempty"`
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
// own engine, and no other container of the pod holds one.
type SecurityContext struct {
	Privileged bool `json:"privileged,omitempty"`
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
// error that refuses the Spec. The caller reads the result from the output claim
// before it runs the teardown, because the teardown removes the claim too.
func (Kubernetes) Render(spec Spec) (Plan, error) {
	if err := spec.Validate(); err != nil {
		return Plan{}, err
	}
	job := renderJob(spec)
	return Plan{
		Job: job,
		Steps: []Step{
			{
				Argv:     []string{kubectl, "--context", spec.Context, "delete", "job", job.Metadata.Name, "--ignore-not-found"},
				Teardown: true,
			},
			{
				// The run's output claim is the caller's to create and its teardown
				// removes it: a Job-only teardown leaves every run's claim behind
				// (gt-7zy61).
				Argv:     []string{kubectl, "--context", spec.Context, "delete", "persistentvolumeclaim", OutClaimName(spec), "--ignore-not-found"},
				Teardown: true,
			},
		},
	}, nil
}

// JSON is the Job as the document kubectl applies.
func (j *Job) JSON() []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(j)
	return buf.Bytes()
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
	if spec.Dind {
		containers = append(containers, dindContainer())
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
				Containers:        containers,
				Volumes:           volumes,
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
		EnvVar{Name: EnvRig, Value: spec.Rig},
		EnvVar{Name: EnvSHA, Value: spec.SHA},
		EnvVar{Name: EnvTiers, Value: strings.Join(spec.TiersOrDefault(), ",")},
		EnvVar{Name: EnvJobs, Value: strconv.Itoa(spec.Jobs)},
	)
	// Requests equal limits, so the scheduler promises the run its whole share
	// instead of letting it burst into a neighbour's (design 2026-10-30).
	share := ResourceList{CPU: strconv.Itoa(spec.CPUs), Memory: strconv.Itoa(spec.MemoryGB) + "Gi"}
	return Container{
		Name:      runnerContainerName,
		Image:     spec.Image,
		Env:       env,
		Resources: &Resources{Requests: share, Limits: share},
		VolumeMounts: []VolumeMount{
			{Name: repoVolume, MountPath: RepoMount, ReadOnly: true},
			{Name: outVolume, MountPath: OutputDir},
		},
	}
}

// dindContainer is the sidecar the packages that need a Docker API reach: the
// pod's only privileged container, bound to the pod's loopback so no other pod
// in the cluster can drive the engine.
func dindContainer() Container {
	share := ResourceList{CPU: dindCPU, Memory: dindMemory}
	return Container{
		Name:  dindContainerName,
		Image: templates.DindImage,
		Args: []string{
			// Without an explicit host dockerd binds 0.0.0.0, which is the pod IP
			// and a privileged Docker API for every pod on the cluster network.
			"--host=tcp://" + dindAddress + ":" + dindPort,
			"--tls=false",
		},
		Env:             []EnvVar{{Name: "DOCKER_TLS_CERTDIR", Value: ""}},
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

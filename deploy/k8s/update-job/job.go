// Package updatejob renders the Kubernetes objects of the pod update's Job on the
// control cluster (gt-1qnr01): one Job and a CronJob named gt-update, suspended,
// that carry the same pod template. The roll runs as a Job, not in the daemon's
// pod, because the daemon's pod is the thing the roll replaces, and a run that
// lived there would die with it. The overseer starts a hand run with
// `kubectl -n gt create job --from=cronjob/gt-update gt-update-manual`, and the
// daemon creates its Jobs from the same template. It renders only, so no apply path
// and no Kubernetes client lives here.
//
// The pod runs the daemon image by digest, the image the Deployment runs now, which
// already holds gt and kubectl. Its init container records the images of one green
// commit with `gt update images`, and its container runs `gt update` against them
// in the cluster the Job runs in, as the ServiceAccount gt-update. The town-state
// claim is mounted read-write, because the run's lock, state and values live under
// <town>/.runtime/update. The viewer token is the only Secret, mounted read-only
// where the dashboard reads it. The RBAC the pod is granted is in
// deploy/k8s/update-rbac.yaml, and nothing here applies it.
package updatejob

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/manifestdata"
)

const (
	// Name names the Job and the CronJob, and labels their pods.
	Name = "gt-update"
	// ServiceAccountName is the ServiceAccount the pod runs as; its Role and
	// RoleBinding are in deploy/k8s/update-rbac.yaml.
	ServiceAccountName = "gt-update"
	// DefaultNamespace is the namespace the objects land in when Values names none:
	// the daemon's, so the pod update's Deployments and the Job share one namespace.
	DefaultNamespace = k8sdaemon.DefaultNamespace
	// PriorityClassName is the class the pod runs at, defined in
	// deploy/k8s/priority-classes.yaml, which sandboxes and CI never preempt.
	PriorityClassName = "gt-control-plane"
	// Schedule is the CronJob's cron expression. The CronJob is suspended, so it
	// never fires; the API needs a schedule to accept it, and @yearly reads as one.
	Schedule = "@yearly"
	// BackoffLimit is zero: the cluster does not retry a failed roll, so a retry is
	// the daemon's decision and not a second pod's.
	BackoffLimit = 0
	// TTLSecondsAfterFinished keeps a finished Job a day, so an operator can read
	// its logs after the run.
	TTLSecondsAfterFinished = 86400
	// InitContainerName and ContainerName name the init container that records the
	// images and the container that rolls the pod onto them.
	InitContainerName = "images"
	ContainerName     = "update"
	// TownStateVolume is the pod volume the town-state claim is mounted by.
	TownStateVolume = "town-state"
	// ViewerTokenVolume is the pod volume the viewer token Secret is mounted by.
	ViewerTokenVolume = "viewer-token"
	// ViewerTokenMode is the Secret's defaultMode: read-only to its owner, and the
	// pod's processes read it as root, as the dashboard does.
	ViewerTokenMode = 0o400
	// ValuesRelPath is the values file the init container writes and the container
	// reads, under the town's .runtime/update directory, which holds the run's state.
	ValuesRelPath = ".runtime/update/images.yaml"
)

// ViewerTokenSecret is the Secret the viewer token is read from: the daemon's
// Secret of that file, which the dashboard also mounts.
const ViewerTokenSecret = k8sdaemon.SecretNamePrefix + k8sdaemon.ViewerTokenFile

// ViewerTokenPath is where the viewer token is mounted in the pod: the file
// `gt update images` reads through XDG_CONFIG_HOME.
const ViewerTokenPath = manifestdata.LiveConfigDirPath + "/" + k8sdaemon.ViewerTokenFile

// ValuesPath is the values file's path in the pod: under the town root the claim is
// mounted at.
const ValuesPath = k8sdaemon.TownRootPath + "/" + ValuesRelPath

var (
	dns1123LabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// imageRefRE is the daemon image reference: a repository ending in
	// @sha256:<64 lowercase hex>. A tag is refused, so the pod runs the image the
	// values file records and not whatever a tag points at today.
	imageRefRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}$`)
	// commitRE is a full 40-character lowercase commit, the form `gt update images
	// --sha` takes.
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Values is what the caller supplies. Image and GreenCommit have no default: the roll
// must name the digest it runs and the commit whose images it records.
type Values struct {
	// Namespace is the namespace the objects land in; DefaultNamespace when empty.
	Namespace string
	// Image is the daemon image by digest, the one the Deployment runs now.
	Image string
	// GreenCommit is the full green commit whose images `gt update images` records.
	GreenCommit string
	// MaxRuntime bounds one run: the pod's activeDeadlineSeconds, which the caller
	// reads from patrols.update.max_runtime. It must be a whole number of seconds.
	MaxRuntime time.Duration
}

// Manifests is what Build returns: the Job, and the CronJob that carries the same
// pod template.
type Manifests struct {
	Job     Job
	CronJob CronJob
}

// Build returns the manifests for v, or the reason v cannot be rendered.
func Build(v Values) (Manifests, error) {
	if v.Namespace == "" {
		v.Namespace = DefaultNamespace
	}
	if err := v.validate(); err != nil {
		return Manifests{}, err
	}
	spec := jobSpec(v)
	return Manifests{
		Job: Job{
			TypeMeta: TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
			Metadata: objectMeta(Name, v.Namespace),
			Spec:     spec,
		},
		CronJob: CronJob{
			TypeMeta: TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
			Metadata: objectMeta(Name, v.Namespace),
			Spec: CronJobSpec{
				Schedule: Schedule,
				// Suspended: the daemon and the overseer create the Jobs, and the
				// CronJob's only use is the template they copy.
				Suspend:     true,
				JobTemplate: JobTemplate{Spec: spec},
			},
		},
	}, nil
}

// validate refuses the values the pod cannot run: an image that is not a digest, a
// namespace that is not a DNS-1123 label, a commit that is not a full sha, and a
// run bound that is not a positive whole number of seconds.
func (v Values) validate() error {
	if !imageRefRE.MatchString(v.Image) {
		return fmt.Errorf("Values.Image %q: want the daemon image by digest (repo@sha256:<64 lowercase hex>) from deploy/values/images.yaml; a tag is refused so the pod runs the image the values file records", v.Image)
	}
	if !dns1123LabelRE.MatchString(v.Namespace) {
		return fmt.Errorf("Values.Namespace %q: want a DNS-1123 label (lowercase letters, digits and '-', starting and ending with a letter or digit)", v.Namespace)
	}
	if !commitRE.MatchString(v.GreenCommit) {
		return fmt.Errorf("Values.GreenCommit %q: want the full 40-character lowercase commit `gt update images --sha` takes", v.GreenCommit)
	}
	if v.MaxRuntime <= 0 {
		return fmt.Errorf("Values.MaxRuntime %s: want a positive duration, the run's activeDeadlineSeconds", v.MaxRuntime)
	}
	if v.MaxRuntime%time.Second != 0 {
		return fmt.Errorf("Values.MaxRuntime %s: want a whole number of seconds, since activeDeadlineSeconds takes seconds", v.MaxRuntime)
	}
	return nil
}

// jobSpec is the Job's spec, and the CronJob's job template. Build gives both the
// one value, so the two pod templates cannot drift apart.
func jobSpec(v Values) JobSpec {
	automount := true
	return JobSpec{
		BackoffLimit:            BackoffLimit,
		ActiveDeadlineSeconds:   int64(v.MaxRuntime / time.Second),
		TTLSecondsAfterFinished: TTLSecondsAfterFinished,
		Template: PodTemplate{
			Metadata: TemplateMeta{Labels: labels()},
			Spec: PodSpec{
				ServiceAccountName:           ServiceAccountName,
				AutomountServiceAccountToken: &automount,
				PriorityClassName:            PriorityClassName,
				RestartPolicy:                "Never",
				InitContainers:               []Container{imagesContainer(v)},
				Containers:                   []Container{updateContainer(v)},
				Volumes: []Volume{
					{Name: TownStateVolume, PersistentVolumeClaim: &PVCVolumeSource{ClaimName: k8sdaemon.TownStateClaim}},
					{Name: ViewerTokenVolume, Secret: &SecretVolumeSource{SecretName: ViewerTokenSecret, DefaultMode: int32Ptr(ViewerTokenMode)}},
				},
			},
		},
	}
}

// imagesContainer records the images of the green commit into the values file. It
// is an init container, so the roll does not start when the record is refused.
func imagesContainer(v Values) Container {
	return Container{
		Name:    InitContainerName,
		Image:   v.Image,
		Command: []string{"gt"},
		Args:    []string{"update", "images", "--sha", v.GreenCommit, "--out", ValuesPath},
		// `gt update images` finds the town from its working directory.
		WorkingDir:   k8sdaemon.TownRootPath,
		Env:          containerEnv(),
		VolumeMounts: mounts(),
	}
}

// updateContainer rolls the daemon's and the dashboard's Deployments onto the images
// the values file records. --pod-in-cluster makes kubectl use the pod's
// ServiceAccount, so no kubeconfig is mounted.
func updateContainer(v Values) Container {
	return Container{
		Name:    ContainerName,
		Image:   v.Image,
		Command: []string{"gt"},
		Args: []string{
			"update", "--images", ValuesPath,
			"--pod-in-cluster", "--pod-namespace", v.Namespace,
		},
		WorkingDir:   k8sdaemon.TownRootPath,
		Env:          containerEnv(),
		VolumeMounts: mounts(),
	}
}

// mounts are the town-state claim read-write and the viewer token read-only, at the
// paths the daemon and the dashboard use.
func mounts() []VolumeMount {
	return []VolumeMount{
		{Name: TownStateVolume, MountPath: k8sdaemon.TownRootPath},
		{Name: ViewerTokenVolume, MountPath: ViewerTokenPath, SubPath: k8sdaemon.ViewerTokenFile, ReadOnly: true},
	}
}

// containerEnv points XDG_CONFIG_HOME at the directory the viewer token is mounted
// under, as the daemon's containers do.
func containerEnv() []EnvVar {
	return []EnvVar{{Name: k8sdaemon.ConfigHomeEnv, Value: manifestdata.LiveOpenShellConfigHome}}
}

// labels are the labels on the Job, the CronJob and their pods. They name the update,
// not the daemon, so the daemon's selector never matches them.
func labels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": Name}
}

// objectMeta is the metadata of an object named name in namespace, with the update's
// labels.
func objectMeta(name, namespace string) ObjectMeta {
	return ObjectMeta{Name: name, Namespace: namespace, Labels: labels()}
}

func int32Ptr(v int32) *int32 { return &v }

// YAML writes v to w as one YAML document. The document is encoded whole before any
// byte reaches w, so a value that cannot be encoded writes nothing, not a partial
// manifest.
func YAML(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := errors.Join(enc.Encode(v), enc.Close()); err != nil {
		return fmt.Errorf("encoding the manifest: %w", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("writing the manifest: %w", err)
	}
	return nil
}

// TypeMeta is the apiVersion and kind of an object.
type TypeMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

// ObjectMeta is the name, namespace and labels of an object.
type ObjectMeta struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace"`
	Labels    map[string]string `yaml:"labels"`
}

// Job is the batch/v1 Job the update runs as.
type Job struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta `yaml:"metadata"`
	Spec     JobSpec    `yaml:"spec"`
}

// CronJob is the batch/v1 CronJob whose job template is the Job's spec.
type CronJob struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta  `yaml:"metadata"`
	Spec     CronJobSpec `yaml:"spec"`
}

// CronJobSpec is the subset of a CronJob's spec the builder sets.
type CronJobSpec struct {
	Schedule    string      `yaml:"schedule"`
	Suspend     bool        `yaml:"suspend"`
	JobTemplate JobTemplate `yaml:"jobTemplate"`
}

// JobTemplate is a CronJob's template for the Jobs it creates.
type JobTemplate struct {
	Spec JobSpec `yaml:"spec"`
}

// JobSpec is the subset of a Job's spec the builder sets.
type JobSpec struct {
	BackoffLimit            int32       `yaml:"backoffLimit"`
	ActiveDeadlineSeconds   int64       `yaml:"activeDeadlineSeconds"`
	TTLSecondsAfterFinished int32       `yaml:"ttlSecondsAfterFinished"`
	Template                PodTemplate `yaml:"template"`
}

// PodTemplate is a pod's metadata and spec.
type PodTemplate struct {
	Metadata TemplateMeta `yaml:"metadata"`
	Spec     PodSpec      `yaml:"spec"`
}

// TemplateMeta is the labels a pod carries.
type TemplateMeta struct {
	Labels map[string]string `yaml:"labels"`
}

// PodSpec is the subset of a pod's spec the builder sets.
type PodSpec struct {
	ServiceAccountName           string      `yaml:"serviceAccountName"`
	AutomountServiceAccountToken *bool       `yaml:"automountServiceAccountToken"`
	PriorityClassName            string      `yaml:"priorityClassName"`
	RestartPolicy                string      `yaml:"restartPolicy"`
	InitContainers               []Container `yaml:"initContainers,omitempty"`
	Containers                   []Container `yaml:"containers"`
	Volumes                      []Volume    `yaml:"volumes"`
}

// Container is the subset of a container the builder sets.
type Container struct {
	Name         string        `yaml:"name"`
	Image        string        `yaml:"image"`
	Command      []string      `yaml:"command"`
	Args         []string      `yaml:"args"`
	WorkingDir   string        `yaml:"workingDir"`
	Env          []EnvVar      `yaml:"env"`
	VolumeMounts []VolumeMount `yaml:"volumeMounts"`
}

// EnvVar is a name and a literal value.
type EnvVar struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// VolumeMount is one path a container mounts a volume at.
type VolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	SubPath   string `yaml:"subPath,omitempty"`
	ReadOnly  bool   `yaml:"readOnly,omitempty"`
}

// Volume is a pod volume: a claim or a Secret.
type Volume struct {
	Name                  string              `yaml:"name"`
	PersistentVolumeClaim *PVCVolumeSource    `yaml:"persistentVolumeClaim,omitempty"`
	Secret                *SecretVolumeSource `yaml:"secret,omitempty"`
}

// PVCVolumeSource names the claim a volume is.
type PVCVolumeSource struct {
	ClaimName string `yaml:"claimName"`
}

// SecretVolumeSource names the Secret a volume is, and the mode its files take.
type SecretVolumeSource struct {
	SecretName  string `yaml:"secretName"`
	DefaultMode *int32 `yaml:"defaultMode,omitempty"`
}

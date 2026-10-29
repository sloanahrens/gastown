package cachevol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// The seed Job is the one place in the town's Kubernetes objects where a container runs as root: it
// fills the shared cache claim from the pinned commits and hands the claim to the sandbox user
// (gt-dv04y). The compile itself runs as that user; root only prepares the claim and hands it over.
const (
	// SeedCacheRoot is where the seed Job mounts the shared claim.
	SeedCacheRoot = "/cache"
	// SeedSourceRoot is where the Job's init container checks the pinned commits out.
	SeedSourceRoot = "/seed"
	// SeedBuildCacheDir and SeedModuleCacheDir name GOCACHE and GOMODCACHE inside the claim. They
	// match the Dockerfile seed RUN's directories, so the entries a sandbox compiles against are the
	// ones the seed wrote.
	SeedBuildCacheDir  = "gocache"
	SeedModuleCacheDir = "gomodcache"
	// SeedMarkerName is the file, in the claim, that records the pins the claim was seeded from: a
	// rerun with the same pins does nothing, and a changed pin reseeds (gt-dv04y).
	SeedMarkerName = ".seed-pins"
	// SeedUserID is the uid and gid the claim belongs to after the seed: the user a sandbox and the
	// trim Job run as.
	SeedUserID = 10001
	// HelperPath is the polecat image's own binary, which carries the containers' helper subcommands
	// (trim-cache, seed-clone, seed-cache).
	HelperPath = "/usr/local/bin/polecat-image"
	// DefaultSeedJobName is the Job's name when the input names none.
	DefaultSeedJobName = "gt-cache-seed"
)

// SeedJobInput is what the seed Job is built from: the polecat image tag its containers run, the
// claim it fills, and the pins it seeds.
type SeedJobInput struct {
	// Name is the Job's name; DefaultSeedJobName when empty.
	Name string
	// Namespace is the Job's namespace; empty leaves the choice to the applying command.
	Namespace string
	// Image is the polecat image tag, which carries git, the Go toolchain and the helper binary.
	Image string
	// ClaimName is the shared cache claim the Job fills.
	ClaimName string
	// Pins is at least one rig and commit, in seed order.
	Pins []Pin
}

// SeedJob is the seed Job as the values kubectl applies. It is a copy of batch/v1's Job, so this
// package still carries no client: the applying command encodes what it is given.
type SeedJob struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       JobSpec    `json:"spec"`
}

// ObjectMeta is the subset of an object's metadata the Job sets.
type ObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// JobSpec is the subset of a Job's spec the seed sets.
type JobSpec struct {
	BackoffLimit *int32      `json:"backoffLimit,omitempty"`
	Template     PodTemplate `json:"template"`
}

// PodTemplate is the Job's pod template.
type PodTemplate struct {
	Metadata TemplateMeta `json:"metadata"`
	Spec     PodSpec      `json:"spec"`
}

// TemplateMeta is the template's labels; a pod template carries no name of its own.
type TemplateMeta struct {
	Labels map[string]string `json:"labels,omitempty"`
}

// PodSpec is the subset of a pod's spec the seed sets.
type PodSpec struct {
	RestartPolicy   string              `json:"restartPolicy"`
	SecurityContext *PodSecurityContext `json:"securityContext,omitempty"`
	InitContainers  []Container         `json:"initContainers,omitempty"`
	Containers      []Container         `json:"containers"`
	Volumes         []Volume            `json:"volumes"`
}

// PodSecurityContext is the Job's root identity: the pod runs as root so the seed can hand the
// claim it wrote to SeedUserID.
type PodSecurityContext struct {
	RunAsUser  *int64 `json:"runAsUser,omitempty"`
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`
}

// Container is one container of the pod.
type Container struct {
	Name         string        `json:"name"`
	Image        string        `json:"image"`
	Command      []string      `json:"command"`
	Args         []string      `json:"args,omitempty"`
	Env          []EnvVar      `json:"env,omitempty"`
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`
}

// EnvVar is one environment variable.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// VolumeMount is one mount of a container.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// Volume is one volume of the pod, named by exactly one source.
type Volume struct {
	Name                  string                    `json:"name"`
	PersistentVolumeClaim *PersistentVolumeClaimRef `json:"persistentVolumeClaim,omitempty"`
	EmptyDir              *EmptyDirVolumeSource     `json:"emptyDir,omitempty"`
}

// PersistentVolumeClaimRef names the claim a volume mounts.
type PersistentVolumeClaimRef struct {
	ClaimName string `json:"claimName"`
}

// EmptyDirVolumeSource is a scratch directory that lives as long as the pod.
type EmptyDirVolumeSource struct{}

// BuildSeedJob returns the seed Job for in, or the reason it cannot be rendered.
//
// A pin is a reviewed commit, because the Job compiles it into a cache every sandbox reads: the
// builder refuses a clone URL that carries credentials, and the seed container runs each compile as
// SeedUserID, so root is left with the claim's handover.
func BuildSeedJob(in SeedJobInput) (*SeedJob, error) {
	name := in.Name
	if name == "" {
		name = DefaultSeedJobName
	}
	if !validDNS1123Label(name) {
		return nil, fmt.Errorf("the seed Job name %q: want a DNS-1123 label of at most 63 characters, since Kubernetes names the pod's job-name label after it", name)
	}
	if in.Namespace != "" && !validDNS1123Label(in.Namespace) {
		return nil, fmt.Errorf("the seed Job namespace %q: want a DNS-1123 label of at most 63 characters", in.Namespace)
	}
	if !validImage(in.Image) {
		return nil, fmt.Errorf("the seed Job image %q: want an image reference such as gastown-polecat:dev", in.Image)
	}
	if !validDNS1123Label(in.ClaimName) {
		return nil, fmt.Errorf("the seed Job claim %q: want a DNS-1123 label of at most 63 characters", in.ClaimName)
	}
	if len(in.Pins) == 0 {
		return nil, errors.New("the seed Job names no pin, so it would seed nothing")
	}
	if _, err := PlanSeed(in.Pins); err != nil {
		return nil, err
	}
	if err := CheckCloneURLs(in.Pins); err != nil {
		return nil, err
	}
	pins := pinsJSON(in.Pins)

	return &SeedJob{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: ObjectMeta{
			Name:      name,
			Namespace: in.Namespace,
			Labels:    seedLabels(),
		},
		Spec: JobSpec{
			// A failed seed has to report itself: the marker makes an operator's rerun cheap, and a
			// retrying pod would rewrite the same failure into the log many times.
			BackoffLimit: int32Ptr(0),
			Template: PodTemplate{
				Metadata: TemplateMeta{Labels: seedLabels()},
				Spec: PodSpec{
					RestartPolicy:   "Never",
					SecurityContext: &PodSecurityContext{RunAsUser: int64Ptr(0), RunAsGroup: int64Ptr(0)},
					InitContainers:  []Container{cloneContainer(in.Image, pins)},
					Containers:      []Container{seedContainer(in.Image, pins)},
					Volumes: []Volume{
						{Name: "cache", PersistentVolumeClaim: &PersistentVolumeClaimRef{ClaimName: in.ClaimName}},
						{Name: "source", EmptyDir: &EmptyDirVolumeSource{}},
					},
				},
			},
		},
	}, nil
}

// JSON returns the Job as the document kubectl applies: indented, and with no HTML escaping, so a
// clone URL's query is readable where an operator checks it. It takes no error, because encoding
// strings, integers and slices of them cannot fail.
func (j *SeedJob) JSON() []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(j)
	return buf.Bytes()
}

// SeedMarker is the marker content for pins: one "<rig> <commit>" line per pin, in pin order. A
// rerun compares the file with this, so the same pins do nothing and a changed pin reseeds.
func SeedMarker(pins []Pin) string {
	var b strings.Builder
	for _, p := range pins {
		b.WriteString(p.Rig)
		b.WriteByte(' ')
		b.WriteString(p.Commit)
		b.WriteByte('\n')
	}
	return b.String()
}

// CheckCloneURLs refuses a pin whose clone URL the init container cannot use: not http(s), no host,
// or carrying userinfo. A URL with userinfo is a credential, and the error never quotes it, so a
// secret reaches neither a manifest nor a log.
func CheckCloneURLs(pins []Pin) error {
	for _, p := range pins {
		if err := checkCloneURL(p.URL); err != nil {
			return fmt.Errorf("the seed pin for %s: %w", p.Rig, err)
		}
	}
	return nil
}

// checkCloneURL is CheckCloneURLs for one URL: nothing in an error is taken from raw, since a URL
// the parser refused can hold a password.
func checkCloneURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("the clone URL is empty; the seed Job clones each pin from Forgejo")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("the clone URL is not an http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("the clone URL scheme %q: the seed Job clones over http or https", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("the clone URL names no host")
	}
	if u.User != nil {
		return errors.New("the clone URL carries userinfo: the seed Job clones without credentials, so its URL must not hold one")
	}
	return nil
}

// validImage reports whether raw can be an image reference: a non-empty name of the characters a
// registry, repository, tag and digest use, at most 512 bytes.
func validImage(raw string) bool {
	if raw == "" || len(raw) > 512 {
		return false
	}
	for i, r := range raw {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-' || r == '/' || r == ':' || r == '@' || r == '+'
		if !ok {
			return false
		}
		if i == 0 && (r == '.' || r == '-' || r == '/' || r == ':' || r == '@' || r == '+') {
			return false
		}
	}
	return true
}

// seedLabels are the labels on the Job and its pods: what a selector matches and what a cleanup
// removes.
func seedLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "polecat-cache-seed",
		"app.kubernetes.io/managed-by": "gt",
	}
}

// pinsJSON is the pins as the argv element both containers take: the init container clones from it
// and the seed container compares the marker with it. No HTML escaping, so a URL's "&" stays
// readable in the manifest; encoding cannot fail, as with JSON.
func pinsJSON(pins []Pin) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(pins)
	return strings.TrimSuffix(buf.String(), "\n")
}

// cloneContainer checks each pinned commit out under SeedSourceRoot. It runs before the seed and as
// the pod's user, so a clone a pin's URL cannot serve fails the Job before the claim is touched.
func cloneContainer(image, pins string) Container {
	return Container{
		Name:    "clone",
		Image:   image,
		Command: []string{HelperPath, "seed-clone"},
		Args:    []string{"--pins", pins, "--dest", SeedSourceRoot},
		// A Forgejo that asks for credentials gets no prompt: the Job fails with the URL's reason
		// instead of hanging until its deadline.
		Env:          []EnvVar{{Name: "GIT_TERMINAL_PROMPT", Value: "0"}},
		VolumeMounts: []VolumeMount{{Name: "source", MountPath: SeedSourceRoot}},
	}
}

// seedContainer runs the PlanSeed steps per rig, hands the claim to SeedUserID, and writes the
// marker. Its image carries the Go toolchain, and GOFLAGS matches the Dockerfile seed RUN's, so the
// entries a sandbox compiles against are the ones the image build used to write.
func seedContainer(image, pins string) Container {
	return Container{
		Name:    "seed",
		Image:   image,
		Command: []string{HelperPath, "seed-cache"},
		Args:    []string{"--pins", pins, "--root", SeedCacheRoot, "--source", SeedSourceRoot},
		Env: []EnvVar{
			{Name: "GOCACHE", Value: path.Join(SeedCacheRoot, SeedBuildCacheDir)},
			{Name: "GOMODCACHE", Value: path.Join(SeedCacheRoot, SeedModuleCacheDir)},
			{Name: "GOFLAGS", Value: "-p=4 -modcacherw"},
		},
		VolumeMounts: []VolumeMount{
			{Name: "cache", MountPath: SeedCacheRoot},
			{Name: "source", MountPath: SeedSourceRoot},
		},
	}
}

func int32Ptr(v int32) *int32 { return &v }

func int64Ptr(v int64) *int64 { return &v }

// Package forgejorunner renders the Kubernetes objects that run the landing
// gate's Forgejo runner on the control cluster (gt-67qco): the Deployment and
// its docker-in-docker sidecar, the ConfigMap holding config.yml, and the two
// claims that keep the engine's images and cache volumes and the runner's
// registration across pod restarts. It renders only, so no apply path and no
// Kubernetes client lives here.
//
// The renderer pins the runner image at Version and the engine at DindTag,
// gives the pod requests equal to limits (runner DefaultRunnerCPU and
// DefaultRunnerMemory, engine DefaultDindCPU and DefaultDindMemory), and
// defaults the engine claim to DefaultDindStorage. Values may override the
// shares, both claim sizes, the capacity and the labels; the labels and the
// Forgejo host address have no default, because a runner with no label takes no
// job and a pod that cannot resolve forgejo cannot register.
//
// An operator verifies after applying what this package cannot: that the engine
// issues its client certificate and the runner's TLS handshake succeeds, that a
// job container mounts the three cache volumes, and that the forgejo alias
// reaches Forgejo from the pod and from its job containers.
package forgejorunner

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// Version is the Forgejo runner release the Deployment pins, exactly. It is a
	// constant and not an input, so no caller can render a different release.
	Version = "13.2.0"
	// Image is the runner container's image.
	Image = runnerRepository + ":" + Version
	// DindTag is the tag the engine sidecar's image carries in the registry.
	DindTag = "dind"
	// DindImage is the docker-in-docker sidecar's image.
	DindImage = dindRepository + ":" + DindTag
	// Name is the name of the Deployment and the stem of the claims' and the
	// ConfigMap's names.
	Name = "forgejo-runner"
	// DefaultNamespace is the namespace the objects land in when Values names none.
	DefaultNamespace = "forgejo"
	// PriorityClassName is the class the pod runs at, defined in
	// deploy/k8s/priority-classes.yaml.
	PriorityClassName = "gt-ci-gate"
	// ForgejoHostname is the name the pod's hostAliases and the job containers'
	// --add-host give Values.ForgejoHostIP.
	ForgejoHostname = "forgejo"
	// RunnerTimeout is the longest a job may run.
	RunnerTimeout = "3h"

	// DefaultCapacity is the most jobs the runner takes at once when Values sets
	// none.
	DefaultCapacity = 1
	// DefaultDindStorage is the engine claim's size when Values names none: large
	// enough for the pulled images and the three cache volumes.
	DefaultDindStorage = "40Gi"
	// DefaultRunnerStorage is the runner data claim's size when Values names none.
	// It holds the registration result and little else.
	DefaultRunnerStorage = "1Gi"
	// DefaultRunnerCPU and DefaultRunnerMemory are the runner container's share
	// when Values name none.
	DefaultRunnerCPU    = "1"
	DefaultRunnerMemory = "1Gi"
	// DefaultDindCPU and DefaultDindMemory are the engine sidecar's share when
	// Values name none.
	DefaultDindCPU    = "6"
	DefaultDindMemory = "12Gi"
)

const (
	runnerRepository = "data.forgejo.org/forgejo/runner"
	dindRepository   = "data.forgejo.org/oci/docker"

	dindPort         = 2376
	dindAddress      = "127.0.0.1"
	certsDir         = "/certs"
	certsClientDir   = certsDir + "/client"
	dindDataDir      = "/var/lib/docker"
	runnerDataDir    = "/data"
	configDir        = "/config"
	configKey        = "config.yml"
	containerNetwork = "host"

	// runnerFileName is where the operator's register step leaves the
	// registration result: under the runner data mount, so the claim keeps it and
	// no manifest carries it.
	runnerFileName = ".runner"

	runnerContainerName = "runner"
	dindContainerName   = "dind"

	// The pod's volumes: each claim, the certificate directory the sidecar and
	// the runner share, and the ConfigMap.
	dindDataVolume   = "dind-data"
	runnerDataVolume = "data"
	certsVolume      = "docker-certs"
	configVolume     = "config"

	// cacheMountRoot is the ci user's home in the job image: the account the live
	// runner image gives jobs, whose home holds the Go and npm caches.
	cacheMountRoot   = "/home/ci"
	gomodMountPath   = cacheMountRoot + "/go/pkg/mod"
	gocacheMountPath = cacheMountRoot + "/.cache/go-build"
	npmMountPath     = cacheMountRoot + "/.npm"

	gomodCacheVolume = "ci-gomod"
	gocacheVolume    = "ci-gocache"
	npmCacheVolume   = "ci-npm"
)

var (
	dns1123LabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// storageRE is the whole grammar this builder accepts: a non-zero whole number
	// and one of the four binary suffixes. It is narrower than Kubernetes' own
	// quantity grammar on purpose, and the error names what it accepts.
	storageRE = regexp.MustCompile(`^[1-9][0-9]*(Ki|Mi|Gi|Ti)$`)
)

// cacheMounts pairs each cache volume with the path a job container mounts it at.
// The names match the live host runner's, so a job moves between the two runners
// without changing where it reads its caches.
var cacheMounts = []struct {
	volume string
	path   string
}{
	{gomodCacheVolume, gomodMountPath},
	{gocacheVolume, gocacheMountPath},
	{npmCacheVolume, npmMountPath},
}

// Share is a container's whole resource share. Requests equal limits, so the
// scheduler promises the container the share rather than allowing it to burst
// into a neighbour's.
type Share struct {
	CPU    string
	Memory string
}

// Values is what the caller supplies. Labels and ForgejoHostIP have no default:
// a runner with no label takes no job, and a pod that cannot resolve forgejo
// cannot register.
type Values struct {
	// Namespace is the namespace every object lands in; DefaultNamespace when empty.
	Namespace string
	// Labels are the runner's labels, each "name:docker://image".
	Labels []string
	// Capacity is the most jobs the runner runs at once; DefaultCapacity when zero.
	Capacity int
	// ForgejoHostIP is the address the name forgejo resolves to in the pod and in
	// its job containers, such as the host gateway 192.168.5.2.
	ForgejoHostIP string
	// DindStorage is the engine claim's requested size; DefaultDindStorage when empty.
	DindStorage string
	// RunnerStorage is the runner data claim's requested size; DefaultRunnerStorage
	// when empty.
	RunnerStorage string
	// RunnerResources is the runner container's share; each empty field takes its
	// default.
	RunnerResources Share
	// DindResources is the engine sidecar's share; each empty field takes its
	// default.
	DindResources Share
}

// Manifests is what Build returns, in the order Render writes them.
type Manifests struct {
	DindClaim   PersistentVolumeClaim
	RunnerClaim PersistentVolumeClaim
	ConfigMap   ConfigMap
	Deployment  Deployment
}

// Build returns the manifests for v, or the reason v cannot be rendered.
func Build(v Values) (Manifests, error) {
	v = v.withDefaults()
	if err := v.validate(); err != nil {
		return Manifests{}, err
	}
	namespace := v.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}

	return Manifests{
		DindClaim:   claim(Name+"-dind", namespace, v.DindStorage),
		RunnerClaim: claim(Name+"-data", namespace, v.RunnerStorage),
		ConfigMap: ConfigMap{
			TypeMeta: TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			Metadata: objectMeta(Name+"-config", namespace),
			Data:     map[string]string{configKey: configYAML(v)},
		},
		Deployment: Deployment{
			TypeMeta: TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			Metadata: objectMeta(Name, namespace),
			Spec: DeploymentSpec{
				// One replica: two would share the engine claim, and one
				// registration file.
				Replicas: 1,
				Selector: LabelSelector{MatchLabels: selectorLabels()},
				Template: PodTemplate{
					Metadata: TemplateMeta{Labels: labels()},
					Spec: PodSpec{
						PriorityClassName: PriorityClassName,
						HostAliases: []HostAlias{{
							IP:        v.ForgejoHostIP,
							Hostnames: []string{ForgejoHostname},
						}},
						Containers: []Container{runnerContainer(v), dindContainer(v)},
						Volumes: []Volume{
							{Name: configVolume, ConfigMap: &ConfigMapVolumeSource{Name: Name + "-config"}},
							// The engine writes its TLS material here and the runner
							// reads the client half; both die with the pod, so the
							// certificates are never committed.
							{Name: certsVolume, EmptyDir: &EmptyDirVolumeSource{}},
							{Name: runnerDataVolume, PersistentVolumeClaim: &PersistentVolumeClaimRef{ClaimName: Name + "-data"}},
							{Name: dindDataVolume, PersistentVolumeClaim: &PersistentVolumeClaimRef{ClaimName: Name + "-dind"}},
						},
					},
				},
			},
		},
	}, nil
}

// claim is a ReadWriteOnce claim of size, named name in namespace.
func claim(name, namespace, size string) PersistentVolumeClaim {
	return PersistentVolumeClaim{
		TypeMeta: TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		Metadata: objectMeta(name, namespace),
		Spec: PVCSpec{
			AccessModes: []string{"ReadWriteOnce"},
			Resources:   ResourceRequirements{Requests: map[string]string{"storage": size}},
		},
	}
}

// runnerContainer is the container that registers with Forgejo and runs jobs.
func runnerContainer(v Values) Container {
	return Container{
		Name:    runnerContainerName,
		Image:   Image,
		Command: []string{"forgejo-runner", "--config", configDir + "/" + configKey, "daemon"},
		// The registration file is relative, so the runner must resolve it under
		// the data mount.
		WorkingDir: runnerDataDir,
		Env: []EnvVar{
			// The sidecar shares the pod's network namespace, so the client
			// reaches the engine on loopback.
			{Name: "DOCKER_HOST", Value: "tcp://" + dindAddress + ":" + strconv.Itoa(dindPort)},
			{Name: "DOCKER_TLS_VERIFY", Value: "1"},
			{Name: "DOCKER_CERT_PATH", Value: certsClientDir},
		},
		Resources: containerResources(v.RunnerResources),
		VolumeMounts: []VolumeMount{
			// The registration result is the only thing the runner writes.
			{Name: runnerDataVolume, MountPath: runnerDataDir},
			{Name: certsVolume, MountPath: certsDir, ReadOnly: true},
			{Name: configVolume, MountPath: configDir, ReadOnly: true},
		},
	}
}

// dindContainer is the pod's only privileged container: the engine that runs the
// gate's job containers.
func dindContainer(v Values) Container {
	return Container{
		Name:  dindContainerName,
		Image: DindImage,
		// The image's entrypoint generates the CA and a client certificate into
		// the shared directory and starts dockerd with TLS verification.
		Env:             []EnvVar{{Name: "DOCKER_TLS_CERTDIR", Value: certsDir}},
		Resources:       containerResources(v.DindResources),
		SecurityContext: &SecurityContext{Privileged: true},
		VolumeMounts: []VolumeMount{
			{Name: certsVolume, MountPath: certsDir},
			{Name: dindDataVolume, MountPath: dindDataDir},
		},
	}
}

// containerResources returns share as both the requests and the limits.
func containerResources(share Share) ContainerResources {
	list := ResourceList{CPU: share.CPU, Memory: share.Memory}
	return ContainerResources{Requests: list, Limits: list}
}

// objectMeta is the metadata every object carries. Each call builds its own
// label map, so editing one object's labels leaves the others alone.
func objectMeta(name, namespace string) ObjectMeta {
	return ObjectMeta{Name: name, Namespace: namespace, Labels: labels()}
}

// labels are on every object; selectorLabels is the subset a selector matches.
func labels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       Name,
		"app.kubernetes.io/managed-by": "gt",
	}
}

// selectorLabels are what the Deployment selects its pods by, so they carry only
// the name.
func selectorLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": Name}
}

func (v Values) withDefaults() Values {
	if v.Capacity == 0 {
		v.Capacity = DefaultCapacity
	}
	if v.DindStorage == "" {
		v.DindStorage = DefaultDindStorage
	}
	if v.RunnerStorage == "" {
		v.RunnerStorage = DefaultRunnerStorage
	}
	if v.RunnerResources.CPU == "" {
		v.RunnerResources.CPU = DefaultRunnerCPU
	}
	if v.RunnerResources.Memory == "" {
		v.RunnerResources.Memory = DefaultRunnerMemory
	}
	if v.DindResources.CPU == "" {
		v.DindResources.CPU = DefaultDindCPU
	}
	if v.DindResources.Memory == "" {
		v.DindResources.Memory = DefaultDindMemory
	}
	return v
}

func (v Values) validate() error {
	if v.Namespace != "" && !validDNS1123Label(v.Namespace) {
		return fmt.Errorf("the namespace %q: want a DNS-1123 label of at most 63 characters", v.Namespace)
	}
	if len(v.Labels) == 0 {
		return errors.New("Values.Labels is empty: name at least one label a job selects the runner by")
	}
	for i, label := range v.Labels {
		if label == "" {
			return fmt.Errorf("Values.Labels[%d] is empty", i)
		}
		if strings.ContainsAny(label, " \t\r\n") {
			return fmt.Errorf("Values.Labels[%d] %q contains whitespace", i, label)
		}
	}
	switch {
	case v.ForgejoHostIP == "":
		return errors.New("Values.ForgejoHostIP is empty: name the host gateway address the forgejo alias resolves to")
	case strings.ContainsAny(v.ForgejoHostIP, " \t\r\n"):
		return fmt.Errorf("Values.ForgejoHostIP %q contains whitespace", v.ForgejoHostIP)
	case !validIPv4(v.ForgejoHostIP):
		return fmt.Errorf("Values.ForgejoHostIP %q: want a dotted IPv4 address such as 192.168.5.2", v.ForgejoHostIP)
	}
	if v.Capacity < 0 {
		return fmt.Errorf("Values.Capacity %d: want a job count of zero or more", v.Capacity)
	}
	if !storageRE.MatchString(v.DindStorage) {
		return fmt.Errorf("the engine claim storage %q: want a whole number and one of Ki, Mi, Gi or Ti, such as 40Gi", v.DindStorage)
	}
	if !storageRE.MatchString(v.RunnerStorage) {
		return fmt.Errorf("the runner data claim storage %q: want a whole number and one of Ki, Mi, Gi or Ti, such as 1Gi", v.RunnerStorage)
	}
	return nil
}

// validDNS1123Label reports whether name is a DNS-1123 label: lowercase
// alphanumerics and interior hyphens, at most 63 characters.
func validDNS1123Label(name string) bool {
	return len(name) <= 63 && dns1123LabelRE.MatchString(name)
}

// validIPv4 reports whether s is a dotted-quad IPv4 address with each octet at
// most 255.
func validIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if len(part) == 0 || len(part) > 3 {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
		if n, err := strconv.Atoi(part); err != nil || n > 255 {
			return false
		}
	}
	return true
}

// configYAML returns config.yml for v. It is written as text rather than
// encoded from a struct, so every value is quoted as a YAML scalar of its own
// and a label's text cannot change the document's shape.
func configYAML(v Values) string {
	var b strings.Builder
	b.WriteString("runner:\n")
	fmt.Fprintf(&b, "  file: %s\n", yamlScalar(runnerFileName))
	fmt.Fprintf(&b, "  capacity: %d\n", v.Capacity)
	fmt.Fprintf(&b, "  timeout: %s\n", yamlScalar(RunnerTimeout))
	b.WriteString("  labels:\n")
	for _, label := range v.Labels {
		fmt.Fprintf(&b, "    - %s\n", yamlScalar(label))
	}
	b.WriteString("container:\n")
	fmt.Fprintf(&b, "  network: %s\n", yamlScalar(containerNetwork))
	fmt.Fprintf(&b, "  options: %s\n", yamlScalar(containerOptions(v)))
	b.WriteString("  valid_volumes:\n")
	for _, mount := range cacheMounts {
		fmt.Fprintf(&b, "    - %s\n", yamlScalar(mount.volume))
	}
	return b.String()
}

// containerOptions is what every job container is started with: the forgejo
// alias its own /etc/hosts does not inherit from the pod, and the three cache
// volumes.
func containerOptions(v Values) string {
	parts := make([]string, 0, 1+2*len(cacheMounts))
	parts = append(parts, "--add-host="+ForgejoHostname+":"+v.ForgejoHostIP)
	for _, mount := range cacheMounts {
		parts = append(parts, "-v", mount.volume+":"+mount.path)
	}
	return strings.Join(parts, " ")
}

// yamlScalar quotes s as a single-quoted YAML scalar: the one form that carries
// any string, since the single quote is its only escape, doubled.
func yamlScalar(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Render writes m to w as one multi-document YAML stream, or returns the reason
// the stream could not be written.
func Render(w io.Writer, m Manifests) error {
	return encode(w, m.DindClaim, m.RunnerClaim, m.ConfigMap, m.Deployment)
}

// YAML writes v to w as one YAML document, or returns the reason it could not
// be written.
func YAML(w io.Writer, v any) error {
	return encode(w, v)
}

// encode writes each document in order. Close reports a failed document's write
// error a second time, so the first error is the one kept.
func encode(w io.Writer, docs ...any) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	var err error
	for _, doc := range docs {
		if err = enc.Encode(doc); err != nil {
			break
		}
	}
	if closeErr := enc.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("encoding the manifest: %w", err)
	}
	return nil
}

// TypeMeta is the apiVersion and kind every object carries.
type TypeMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

// ObjectMeta is the subset of an object's metadata the manifests set.
type ObjectMeta struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

// TemplateMeta is a pod template's metadata: labels, and no name or namespace.
type TemplateMeta struct {
	Labels map[string]string `yaml:"labels"`
}

// LabelSelector is the subset of a selector the Deployment sets.
type LabelSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

// Deployment is the subset of apps/v1's Deployment the builder renders.
type Deployment struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta     `yaml:"metadata"`
	Spec     DeploymentSpec `yaml:"spec"`
}

// DeploymentSpec is the subset of a Deployment's spec the builder sets.
type DeploymentSpec struct {
	Replicas int32         `yaml:"replicas"`
	Selector LabelSelector `yaml:"selector"`
	Template PodTemplate   `yaml:"template"`
}

// PodTemplate is the pod the Deployment creates.
type PodTemplate struct {
	Metadata TemplateMeta `yaml:"metadata"`
	Spec     PodSpec      `yaml:"spec"`
}

// PodSpec is the subset of a pod's spec the builder sets.
type PodSpec struct {
	PriorityClassName string      `yaml:"priorityClassName,omitempty"`
	HostAliases       []HostAlias `yaml:"hostAliases,omitempty"`
	Containers        []Container `yaml:"containers"`
	Volumes           []Volume    `yaml:"volumes"`
}

// HostAlias is one /etc/hosts entry the pod's containers share: every hostname
// resolves to IP.
type HostAlias struct {
	IP        string   `yaml:"ip"`
	Hostnames []string `yaml:"hostnames"`
}

// Container is one container of the pod.
type Container struct {
	Name            string             `yaml:"name"`
	Image           string             `yaml:"image"`
	Command         []string           `yaml:"command,omitempty"`
	WorkingDir      string             `yaml:"workingDir,omitempty"`
	Env             []EnvVar           `yaml:"env,omitempty"`
	Resources       ContainerResources `yaml:"resources"`
	SecurityContext *SecurityContext   `yaml:"securityContext,omitempty"`
	VolumeMounts    []VolumeMount      `yaml:"volumeMounts,omitempty"`
}

// EnvVar is one environment variable.
type EnvVar struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// ContainerResources is a container's requests and limits, given identically.
type ContainerResources struct {
	Requests ResourceList `yaml:"requests"`
	Limits   ResourceList `yaml:"limits"`
}

// ResourceList is the CPU and memory quantities of a ContainerResources.
type ResourceList struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

// SecurityContext is the capability a container needs: the dind sidecar runs
// the pod's only privileged container.
type SecurityContext struct {
	Privileged bool `yaml:"privileged,omitempty"`
}

// VolumeMount is one mount of a container.
type VolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	ReadOnly  bool   `yaml:"readOnly,omitempty"`
}

// Volume is one volume of the pod, named by exactly one source: a claim, scratch
// space that lives as long as the pod, or a ConfigMap.
type Volume struct {
	Name                  string                    `yaml:"name"`
	PersistentVolumeClaim *PersistentVolumeClaimRef `yaml:"persistentVolumeClaim,omitempty"`
	EmptyDir              *EmptyDirVolumeSource     `yaml:"emptyDir,omitempty"`
	ConfigMap             *ConfigMapVolumeSource    `yaml:"configMap,omitempty"`
}

// PersistentVolumeClaimRef names the claim a volume mounts.
type PersistentVolumeClaimRef struct {
	ClaimName string `yaml:"claimName"`
}

// EmptyDirVolumeSource is scratch space that lives as long as the pod.
type EmptyDirVolumeSource struct{}

// ConfigMapVolumeSource names the ConfigMap a volume mounts.
type ConfigMapVolumeSource struct {
	Name string `yaml:"name"`
}

// ConfigMap is the subset of v1's ConfigMap the builder renders.
type ConfigMap struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta        `yaml:"metadata"`
	Data     map[string]string `yaml:"data"`
}

// PersistentVolumeClaim is the subset of v1's PersistentVolumeClaim the builder
// renders.
type PersistentVolumeClaim struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta `yaml:"metadata"`
	Spec     PVCSpec    `yaml:"spec"`
}

// PVCSpec is the subset of a claim's spec the builder sets.
type PVCSpec struct {
	AccessModes []string             `yaml:"accessModes"`
	Resources   ResourceRequirements `yaml:"resources"`
}

// ResourceRequirements is the claim's requested storage.
type ResourceRequirements struct {
	Requests map[string]string `yaml:"requests"`
}

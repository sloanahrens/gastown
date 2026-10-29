// Package dolt renders the Kubernetes objects that run Dolt: the StatefulSet,
// the headless Service that addresses its pod, and the claim that holds the
// databases (gt-cfjn7). It renders only, so no apply path and no Kubernetes
// client lives here.
package dolt

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// Version is the Dolt release the objects pin, exactly. It is a constant and
	// not an input, so no caller can render a different release (gt-cfjn7).
	Version = "2.3.2"
	// Image is the container image the StatefulSet runs.
	Image = imageRepository + ":" + Version
	// ServicePort is the port the Service publishes: the port every Dolt endpoint
	// in the town reaches a server on (gt-cfjn7).
	ServicePort = 3307
	// ContainerPort is the port the container declares, under PortName.
	ContainerPort = 3306
	// PortName is the one name shared by the container port and the Service's
	// targetPort, so the Service forwards to a name instead of repeating a number.
	PortName = "mysql"
	// DataDir is where the container mounts the claim.
	DataDir = "/var/lib/dolt"
	// Name is the name of all three objects, so the pod is addressable as
	// <Name>-0.<Name> inside the namespace.
	Name = "dolt"
	// DefaultNamespace is the namespace the objects land in when Config names none.
	DefaultNamespace = "openshell"
	// DefaultRootPasswordKey is the key in Config.RootPasswordSecret when Config
	// names none.
	DefaultRootPasswordKey = "root-password"
)

const imageRepository = "dolthub/dolt-sql-server"

var (
	dns1123LabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	secretKeyRE    = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
	// storageRE is the whole grammar this builder accepts: a non-zero whole number
	// and one of the four binary suffixes. It is narrower than Kubernetes' own
	// quantity grammar on purpose, and the error names what it accepts.
	storageRE = regexp.MustCompile(`^[1-9][0-9]*(Ki|Mi|Gi|Ti)$`)
)

// Config is what the caller supplies. Every field that decides who may reach
// the database has no default, so a manifest cannot be rendered into a state
// nobody asked for.
type Config struct {
	// Namespace is the namespace all three objects land in; DefaultNamespace when empty.
	Namespace string
	// Storage is the claim's requested size, such as "10Gi".
	Storage string
	// RootHost is where Dolt accepts root's login from, such as "localhost" or
	// "%". It has no default: the server's own default is every host.
	RootHost string
	// RootPasswordSecret is the Secret holding root's password. The builder
	// renders a reference to it and never reads it.
	RootPasswordSecret string
	// RootPasswordKey is the key in RootPasswordSecret; DefaultRootPasswordKey
	// when empty.
	RootPasswordKey string
}

// Manifests is what Build returns, in the order a fresh namespace needs them.
type Manifests struct {
	Claim       PersistentVolumeClaim
	Service     Service
	StatefulSet StatefulSet
}

// Build returns the manifests for c, or the reason c cannot be rendered.
func Build(c Config) (Manifests, error) {
	if err := c.validate(); err != nil {
		return Manifests{}, err
	}
	namespace := c.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}
	key := c.RootPasswordKey
	if key == "" {
		key = DefaultRootPasswordKey
	}

	return Manifests{
		Claim: PersistentVolumeClaim{
			TypeMeta: TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
			Metadata: objectMeta(namespace),
			Spec: PVCSpec{
				AccessModes: []string{"ReadWriteOnce"},
				Resources:   ResourceRequirements{Requests: map[string]string{"storage": c.Storage}},
			},
		},
		Service: Service{
			TypeMeta: TypeMeta{APIVersion: "v1", Kind: "Service"},
			Metadata: objectMeta(namespace),
			Spec: ServiceSpec{
				// Headless: the cluster DNS answers with the pod's own address,
				// <Name>-0.<Name>, which is what a StatefulSet identity is for.
				ClusterIP: "None",
				Selector:  selectorLabels(),
				Ports: []PublishedPort{{
					Name:       PortName,
					Port:       ServicePort,
					TargetPort: PortName,
					Protocol:   "TCP",
				}},
			},
		},
		StatefulSet: StatefulSet{
			TypeMeta: TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
			Metadata: objectMeta(namespace),
			Spec: StatefulSetSpec{
				ServiceName: Name,
				// One replica: Dolt serves reads and writes from one process, and
				// the claim below is the one volume it writes.
				Replicas: 1,
				Selector: LabelSelector{MatchLabels: selectorLabels()},
				Template: PodTemplate{
					Metadata: TemplateMeta{Labels: labels()},
					Spec: PodSpec{
						Containers: []Container{{
							Name:  Name,
							Image: Image,
							Ports: []Port{{Name: PortName, ContainerPort: ContainerPort}},
							// Root's password comes from the Secret on every render, so
							// even the host "%" admits root only with that password
							// (gt-cfjn7).
							Env: []EnvVar{
								{Name: "DOLT_ROOT_HOST", Value: c.RootHost},
								{Name: "DOLT_ROOT_PASSWORD", ValueFrom: &EnvVarSource{
									SecretKeyRef: &SecretKeySelector{Name: c.RootPasswordSecret, Key: key},
								}},
							},
							VolumeMounts: []VolumeMount{{Name: "data", MountPath: DataDir}},
						}},
						Volumes: []Volume{{
							Name: "data",
							// A standalone claim, not a volumeClaimTemplate: the
							// claim is one object whose size the caller states, and
							// it survives the StatefulSet being deleted.
							PersistentVolumeClaim: &PersistentVolumeClaimRef{ClaimName: Name},
						}},
					},
				},
			},
		},
	}, nil
}

// objectMeta is the metadata every object carries. Each call builds its own
// label map, so editing one object's labels leaves the others alone.
func objectMeta(namespace string) ObjectMeta {
	return ObjectMeta{Name: Name, Namespace: namespace, Labels: labels()}
}

// labels are the labels on every object: what a selector matches and what a
// cleanup removes.
func labels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       Name,
		"app.kubernetes.io/managed-by": "gt",
	}
}

// selectorLabels are what a Service and a StatefulSet select their pods by. A
// selector cannot be updated in place, so it carries only the name.
func selectorLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": Name}
}

func (c Config) validate() error {
	if c.Namespace != "" && !validDNS1123Label(c.Namespace) {
		return fmt.Errorf("the namespace %q: want a DNS-1123 label of at most 63 characters", c.Namespace)
	}
	if !storageRE.MatchString(c.Storage) {
		return fmt.Errorf("the claim storage %q: want a whole number and one of Ki, Mi, Gi or Ti, such as 10Gi", c.Storage)
	}
	if c.RootHost == "" {
		return errors.New("the root host is empty: name the host Dolt accepts root's login from")
	}
	if strings.ContainsAny(c.RootHost, " \t\r\n") {
		return fmt.Errorf("the root host %q contains whitespace", c.RootHost)
	}
	if !validDNS1123Label(c.RootPasswordSecret) {
		return fmt.Errorf("the root password Secret %q: want a DNS-1123 label of at most 63 characters, since root's password comes from a Secret and the builder has no unauthenticated mode", c.RootPasswordSecret)
	}
	if c.RootPasswordKey != "" && !secretKeyRE.MatchString(c.RootPasswordKey) {
		return fmt.Errorf("the root password key %q: want the characters a Secret key allows", c.RootPasswordKey)
	}
	return nil
}

// validDNS1123Label reports whether name is a DNS-1123 label: lowercase
// alphanumerics and interior hyphens, at most 63 characters.
func validDNS1123Label(name string) bool {
	return len(name) <= 63 && dns1123LabelRE.MatchString(name)
}

// Render writes m to w as one multi-document YAML stream, or returns the reason
// the stream could not be written.
func Render(w io.Writer, m Manifests) error {
	return encode(w, m.Claim, m.Service, m.StatefulSet)
}

// YAML writes v to w as one YAML document, or returns the reason it could not
// be written.
func YAML(w io.Writer, v any) error {
	return encode(w, v)
}

// encode writes each document in order. A failed write is the one failure the
// encoder can report for these values, and Close reports the same one, so both
// are joined rather than dropped.
func encode(w io.Writer, docs ...any) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	var encodeErr error
	for _, doc := range docs {
		if encodeErr = enc.Encode(doc); encodeErr != nil {
			break
		}
	}
	if err := errors.Join(encodeErr, enc.Close()); err != nil {
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

// LabelSelector is the subset of a selector the StatefulSet sets.
type LabelSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

// StatefulSet is the subset of apps/v1's StatefulSet the builder renders.
type StatefulSet struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta      `yaml:"metadata"`
	Spec     StatefulSetSpec `yaml:"spec"`
}

// StatefulSetSpec is the subset of a StatefulSet's spec the builder sets.
type StatefulSetSpec struct {
	ServiceName string        `yaml:"serviceName"`
	Replicas    int32         `yaml:"replicas"`
	Selector    LabelSelector `yaml:"selector"`
	Template    PodTemplate   `yaml:"template"`
}

// PodTemplate is the pod the StatefulSet creates.
type PodTemplate struct {
	Metadata TemplateMeta `yaml:"metadata"`
	Spec     PodSpec      `yaml:"spec"`
}

// PodSpec is the subset of a pod's spec the builder sets.
type PodSpec struct {
	Containers []Container `yaml:"containers"`
	Volumes    []Volume    `yaml:"volumes"`
}

// Container is one container of the pod.
type Container struct {
	Name         string        `yaml:"name"`
	Image        string        `yaml:"image"`
	Ports        []Port        `yaml:"ports,omitempty"`
	Env          []EnvVar      `yaml:"env,omitempty"`
	VolumeMounts []VolumeMount `yaml:"volumeMounts,omitempty"`
}

// Port is one port the container declares.
type Port struct {
	Name          string `yaml:"name"`
	ContainerPort int32  `yaml:"containerPort"`
}

// EnvVar is one environment variable: a literal value or a reference to one.
type EnvVar struct {
	Name      string        `yaml:"name"`
	Value     string        `yaml:"value,omitempty"`
	ValueFrom *EnvVarSource `yaml:"valueFrom,omitempty"`
}

// EnvVarSource is where an environment variable's value is read from.
type EnvVarSource struct {
	SecretKeyRef *SecretKeySelector `yaml:"secretKeyRef,omitempty"`
}

// SecretKeySelector names one key in one Secret.
type SecretKeySelector struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

// VolumeMount is one mount of a container.
type VolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
}

// Volume is one volume of the pod.
type Volume struct {
	Name                  string                    `yaml:"name"`
	PersistentVolumeClaim *PersistentVolumeClaimRef `yaml:"persistentVolumeClaim,omitempty"`
}

// PersistentVolumeClaimRef names the claim a volume mounts.
type PersistentVolumeClaimRef struct {
	ClaimName string `yaml:"claimName"`
}

// Service is the subset of v1's Service the builder renders.
type Service struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta  `yaml:"metadata"`
	Spec     ServiceSpec `yaml:"spec"`
}

// ServiceSpec is the subset of a Service's spec the builder sets.
type ServiceSpec struct {
	ClusterIP string            `yaml:"clusterIP"`
	Selector  map[string]string `yaml:"selector"`
	Ports     []PublishedPort   `yaml:"ports"`
}

// PublishedPort is one port the Service publishes.
type PublishedPort struct {
	Name       string `yaml:"name"`
	Port       int32  `yaml:"port"`
	TargetPort string `yaml:"targetPort"`
	Protocol   string `yaml:"protocol"`
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

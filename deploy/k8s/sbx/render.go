package sbx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

const (
	// bootstrapConfigMap is where up.sh records the helm marker the render
	// writes, and the marker's data key.
	bootstrapConfigMap = "gt-bootstrap"
	bootstrapMarkerKey = "helm"
)

// openshellNamespaceDoc is the openshell Namespace: helm's --create-namespace
// makes it during the first install, and the live one carries its name label.
const openshellNamespaceDoc = `apiVersion: v1
kind: Namespace
metadata:
  name: openshell
  labels:
    name: openshell`

// sbxStaticOrder is the apply order of the gt-sbx static objects in the desired
// set, which follows up.sh: the gateway's NodePort, the control-plane Services,
// then the cache claim. SbxStatic returns the Services first, so the order is
// named here rather than taken from that call.
var sbxStaticOrder = []struct{ kind, name string }{
	{"Service", "openshell-nodeport"},
	{"Service", "control-plane"},
	{"Service", "dolt"},
	{"Service", "forgejo"},
	{"Service", "tracker"},
	{"PersistentVolumeClaim", "gt-cache"},
}

// Inputs is what Render reads: the file reader, the helm runner and the three
// pinned input paths.
type Inputs struct {
	// Read reads a file's bytes; Render reads the values file and the
	// agent-sandbox manifest through it.
	Read func(path string) ([]byte, error)
	// Run runs `helm template`; a nil runner is refused by OpenShell.
	Run Runner
	// AgentSandboxPath is the agent-sandbox manifest, which must hold the
	// pinned release's bytes.
	AgentSandboxPath string
	// ChartDir is the pulled OpenShell chart directory and ValuesPath the values
	// file the chart is rendered with.
	ChartDir, ValuesPath string
}

// ObjectRef names one Kubernetes object by kind, namespace and name. Namespace
// is empty for a cluster-scoped kind.
type ObjectRef struct {
	Kind      string
	Namespace string
	Name      string
}

// Set is the gt-sbx desired set: the objects to apply and the Secrets that must
// exist but are never applied.
type Set struct {
	// Objects are the objects in apply order.
	Objects []k8s.StaticObject
	// Secrets names the OpenShell chart's Secrets, which the render never
	// applies.
	Secrets []string
	// Runtime names the objects the sandbox cluster creates itself that carry a
	// fixed name, so the drift compare never reports them as extra. It is a copy
	// of the package list; a test asserts it shares no object with Objects.
	Runtime []ObjectRef
	// RuntimeNamespaces names the namespaces in which every object is one the
	// cluster creates itself, so the drift compare does not report those as
	// extra. It is a copy of the package list.
	RuntimeNamespaces []string
	// RuntimeClusterPrefixes names the name prefixes of the cluster-scoped
	// ClusterRoles and ClusterRoleBindings the cluster creates itself, so the
	// drift compare does not report those as extra. It is a copy of the package
	// list.
	RuntimeClusterPrefixes []string
}

// IsRuntime reports whether the object is one the sandbox cluster creates
// itself, so the drift compare never reports it as extra.
func (s Set) IsRuntime(ref ObjectRef) bool {
	if slices.Contains(s.Runtime, ref) {
		return true
	}
	if ref.Namespace != "" && slices.Contains(s.RuntimeNamespaces, ref.Namespace) {
		return true
	}
	if ref.Namespace == "" && (ref.Kind == "ClusterRole" || ref.Kind == "ClusterRoleBinding") {
		for _, prefix := range s.RuntimeClusterPrefixes {
			if strings.HasPrefix(ref.Name, prefix) {
				return true
			}
		}
	}
	return false
}

// sbxRuntime is every fixed-name object the drift compare must not report as
// extra: those the sandbox cluster creates itself, grouped by the class that
// creates it, and the deployer's own RBAC objects, applied in the sitting. It is
// the source Render fills Set.Runtime from. The cluster's rows were read by hand
// from deploy/k8s/kind's config and up.sh, the pinned metrics-server manifest
// and kind's add-ons, and no live cluster was read for them. The deployer's rows
// are read from deploy/k8s/sbx-deployer-rbac.yaml. A miss shows as an "extra";
// the fix is one more row.
var sbxRuntime = []ObjectRef{
	// The OpenShell gateway and the daemon create these at runtime.
	{Kind: "NetworkPolicy", Namespace: openshellName, Name: "openshell-sandbox-supervisors"},
	{Kind: "NetworkPolicy", Namespace: openshellName, Name: "openshell-sandbox-workloads"},
	// The daemon's cache trim Job, named cacheTrimJobName for the gt-cache claim.
	{Kind: "Job", Namespace: openshellName, Name: "gt-cache-trim-gt-cache"},
	// The chart's certgen hook leaves these behind.
	{Kind: "ServiceAccount", Namespace: openshellName, Name: "openshell-certgen"},
	{Kind: "Role", Namespace: openshellName, Name: "openshell-certgen"},
	{Kind: "RoleBinding", Namespace: openshellName, Name: "openshell-certgen"},

	// The Namespaces kind and kubeadm start with.
	{Kind: "Namespace", Name: "default"},
	{Kind: "Namespace", Name: "kube-system"},
	{Kind: "Namespace", Name: "kube-public"},
	{Kind: "Namespace", Name: "kube-node-lease"},
	{Kind: "Namespace", Name: "local-path-storage"},

	// kind's local-path storage add-on.
	{Kind: "Deployment", Namespace: "local-path-storage", Name: "local-path-provisioner"},
	{Kind: "ConfigMap", Namespace: "local-path-storage", Name: "local-path-config"},
	{Kind: "ServiceAccount", Namespace: "local-path-storage", Name: "local-path-provisioner"},
	{Kind: "Role", Namespace: "local-path-storage", Name: "local-path-provisioner-role"},
	{Kind: "RoleBinding", Namespace: "local-path-storage", Name: "local-path-provisioner-bind"},
	{Kind: "ClusterRole", Name: "local-path-provisioner-role"},
	{Kind: "ClusterRoleBinding", Name: "local-path-provisioner-bind"},
	{Kind: "StorageClass", Name: "standard"},

	// Cluster-scoped objects with fixed names: the apiserver's bootstrap
	// ClusterRoles and ClusterRoleBinding (no system: or kubeadm: prefix, so a
	// prefix rule does not cover them), the PriorityClasses, kindnet, and the
	// metrics-server manifest up.sh applies.
	{Kind: "ClusterRole", Name: "admin"},
	{Kind: "ClusterRole", Name: "cluster-admin"},
	{Kind: "ClusterRole", Name: "edit"},
	{Kind: "ClusterRole", Name: "view"},
	{Kind: "ClusterRoleBinding", Name: "cluster-admin"},
	{Kind: "PriorityClass", Name: "system-cluster-critical"},
	{Kind: "PriorityClass", Name: "system-node-critical"},
	{Kind: "ClusterRole", Name: "kindnet"},
	{Kind: "ClusterRoleBinding", Name: "kindnet"},
	{Kind: "ClusterRole", Name: "system:aggregated-metrics-reader"},
	{Kind: "ClusterRole", Name: "system:metrics-server"},
	{Kind: "ClusterRoleBinding", Name: "metrics-server:system:auth-delegator"},
	{Kind: "ClusterRoleBinding", Name: "system:metrics-server"},
	{Kind: "APIService", Name: "v1beta1.metrics.k8s.io"},

	// The deployer's own RBAC objects: deploy/k8s/sbx-deployer-rbac.yaml, which
	// the operator applies with the admin kubeconfig once per rebuild, in the
	// sitting. The plan and the apply never write it, so no other list names
	// them; without these rows the compare reports every one as extra.
	{Kind: "ClusterRole", Name: "gt-sbx-deployer-policy"},
	{Kind: "ClusterRoleBinding", Name: "gt-sbx-deployer-policy"},
	{Kind: "Role", Namespace: openshellName, Name: "gt-sbx-deployer-apply"},
	{Kind: "RoleBinding", Namespace: openshellName, Name: "gt-sbx-deployer-apply"},
	{Kind: "Role", Namespace: "agent-sandbox-system", Name: "gt-sbx-deployer-apply"},
	{Kind: "RoleBinding", Namespace: "agent-sandbox-system", Name: "gt-sbx-deployer-apply"},
	{Kind: "ClusterRoleBinding", Name: "gt-sbx-deployer-view"},
	{Kind: "ClusterRole", Name: "gt-sbx-deployer-reader-extra"},
	{Kind: "ClusterRoleBinding", Name: "gt-sbx-deployer-reader-extra"},
	{Kind: "Role", Namespace: openshellName, Name: "gt-sbx-deployer-secrets"},
	{Kind: "RoleBinding", Namespace: openshellName, Name: "gt-sbx-deployer-secrets-read"},
	{Kind: "RoleBinding", Namespace: openshellName, Name: "gt-sbx-deployer-secrets-apply"},
}

// sbxRuntimeNamespaces names the namespaces kubeadm and kind create themselves
// and fill with their own objects, which have no fixed name to list: the
// control plane's ConfigMaps, ServiceAccounts, Roles, RoleBindings, the coredns
// Deployment, the kube-dns Service, the kindnet and kube-proxy DaemonSets, and
// the metrics-server objects up.sh installs into kube-system. It is the source
// Render fills Set.RuntimeNamespaces from; a miss shows as an "extra" in the
// drift compare, and the fix is one more row.
var sbxRuntimeNamespaces = []string{"kube-system", "kube-public", "kube-node-lease"}

// sbxRuntimeClusterPrefixes names the prefixes the control plane gives its own
// cluster-scoped ClusterRoles and ClusterRoleBindings. It is the source Render
// fills Set.RuntimeClusterPrefixes from; a miss shows as an "extra" in the drift
// compare, and the fix is one more row.
var sbxRuntimeClusterPrefixes = []string{"system:", "kubeadm:"}

// Render composes the gt-sbx cluster's desired set in apply order from the
// pinned agent-sandbox manifest, the pinned OpenShell chart and the embedded
// static set.
func Render(ctx context.Context, in Inputs) (Set, error) {
	return renderSet(ctx, in, k8s.SbxStatic, AgentSandboxSHA256)
}

// renderSet is Render over an injected static source and agent-sandbox digest,
// so a test renders the trimmed fixtures whose bytes are not the releases'.
func renderSet(ctx context.Context, in Inputs, static func() ([]k8s.StaticObject, error), agentSandboxDigest string) (Set, error) {
	if in.Read == nil {
		return Set{}, errors.New("render the gt-sbx set: no reader, so the values file and the agent-sandbox manifest cannot be read")
	}
	values, err := in.Read(in.ValuesPath)
	if err != nil {
		return Set{}, fmt.Errorf("render the gt-sbx set: reading the values file %s: %w", in.ValuesPath, err)
	}
	sandbox, err := agentSandbox(in.Read, in.AgentSandboxPath, agentSandboxDigest, yaml.Marshal)
	if err != nil {
		return Set{}, fmt.Errorf("render the gt-sbx set: %w", err)
	}
	staticObjects, err := static()
	if err != nil {
		return Set{}, fmt.Errorf("render the gt-sbx set: reading the gt-sbx static set: %w", err)
	}
	chart, err := OpenShell(ctx, in.Run, in.ChartDir, in.ValuesPath)
	if err != nil {
		return Set{}, fmt.Errorf("render the gt-sbx set: %w", err)
	}

	sum := sha256.Sum256(values)
	marker := ChartVersion + ":" + hex.EncodeToString(sum[:])

	// The Namespaces come first: every namespaced object below lives in one.
	var objects, sandboxObjects []k8s.StaticObject
	for _, object := range sandbox {
		if object.Kind == "Namespace" {
			objects = append(objects, object)
			continue
		}
		sandboxObjects = append(sandboxObjects, object)
	}
	objects = append(objects, openshellNamespace())
	for _, object := range staticObjects {
		if object.Kind == "PriorityClass" {
			objects = append(objects, object)
		}
	}
	objects = append(objects, sandboxObjects...)
	objects = append(objects, chart.Objects...)
	for _, want := range sbxStaticOrder {
		object, ok := findStatic(staticObjects, want.kind, want.name)
		if !ok {
			return Set{}, fmt.Errorf("render the gt-sbx set: the static set holds no %s %s, so the desired set would leave it unapplied", want.kind, want.name)
		}
		objects = append(objects, object)
	}
	objects = append(objects, bootstrapConfigMapObject(marker))
	// The desired set is applied in full, and the Secrets are never applied:
	// Set.Secrets names them for the existence check. The chart drops and names
	// its own Secrets and the agent-sandbox manifest refuses the kind, so the
	// static set is checked beside the composed list, before the apply order
	// drops its unplaced objects.
	if err := checkNoSecrets(slices.Concat(objects, staticObjects)); err != nil {
		return Set{}, err
	}
	if err := checkDuplicates(objects); err != nil {
		return Set{}, err
	}
	// A static object this order does not name would otherwise be dropped from
	// the desired set without a word.
	for _, object := range staticObjects {
		if object.Kind == "PriorityClass" || inStaticOrder(object) {
			continue
		}
		return Set{}, fmt.Errorf("render the gt-sbx set: the static set holds %s %s, which the desired set does not place", object.Kind, object.Name)
	}
	return Set{
		Objects:                objects,
		Secrets:                slices.Clone(chart.Secrets),
		Runtime:                slices.Clone(sbxRuntime),
		RuntimeNamespaces:      slices.Clone(sbxRuntimeNamespaces),
		RuntimeClusterPrefixes: slices.Clone(sbxRuntimeClusterPrefixes),
	}, nil
}

// checkNoSecrets refuses an object of kind Secret. The desired set is applied
// in full, while the Secrets must exist and are never applied, so a Secret in
// the composed list or in a source feeding it is a refusal rather than an
// object the applier would create.
func checkNoSecrets(objects []k8s.StaticObject) error {
	for _, object := range objects {
		if object.Kind == "Secret" {
			return fmt.Errorf("render the gt-sbx set: the desired set holds %s, which is never applied", objectKey(object))
		}
	}
	return nil
}

// checkDuplicates refuses two objects with the same kind, namespace and name,
// naming the one that repeats.
func checkDuplicates(objects []k8s.StaticObject) error {
	seen := make(map[string]bool, len(objects))
	for _, object := range objects {
		key := objectKey(object)
		if seen[key] {
			return fmt.Errorf("render the gt-sbx set: duplicate object %s", key)
		}
		seen[key] = true
	}
	return nil
}

// objectKey is the identity a duplicate is found and named by.
func objectKey(object k8s.StaticObject) string {
	if object.Namespace == "" {
		return object.Kind + "/" + object.Name
	}
	return object.Kind + "/" + object.Namespace + "/" + object.Name
}

// openshellNamespace is the openshell Namespace object the render builds.
func openshellNamespace() k8s.StaticObject {
	return k8s.StaticObject{
		APIVersion: "v1",
		Kind:       "Namespace",
		Name:       openshellName,
		Cluster:    true,
		Doc:        []byte(openshellNamespaceDoc),
	}
}

// bootstrapConfigMapObject is the ConfigMap up.sh writes the marker into, with
// helm set to the chart version and the values file's digest.
func bootstrapConfigMapObject(marker string) k8s.StaticObject {
	doc := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  %s: %q
`, bootstrapConfigMap, openshellName, bootstrapMarkerKey, marker)
	return k8s.StaticObject{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       bootstrapConfigMap,
		Namespace:  openshellName,
		Doc:        []byte(doc),
	}
}

// findStatic returns the static object with kind and name, and whether the set
// holds one.
func findStatic(static []k8s.StaticObject, kind, name string) (k8s.StaticObject, bool) {
	for _, object := range static {
		if object.Kind == kind && object.Name == name {
			return object, true
		}
	}
	return k8s.StaticObject{}, false
}

// inStaticOrder reports whether the object is one sbxStaticOrder names.
func inStaticOrder(object k8s.StaticObject) bool {
	for _, want := range sbxStaticOrder {
		if object.Kind == want.kind && object.Name == want.name {
			return true
		}
	}
	return false
}

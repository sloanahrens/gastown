package sbx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

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

// Set is the gt-sbx desired set: the objects to apply and the Secrets that must
// exist but are never applied.
type Set struct {
	// Objects are the objects in apply order.
	Objects []k8s.StaticObject
	// Secrets names the OpenShell chart's Secrets, which the render never
	// applies.
	Secrets []string
}

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
	// A static object this order does not name would otherwise be dropped from
	// the desired set without a word.
	for _, object := range staticObjects {
		if object.Kind == "PriorityClass" || inStaticOrder(object) {
			continue
		}
		return Set{}, fmt.Errorf("render the gt-sbx set: the static set holds %s %s, which the desired set does not place", object.Kind, object.Name)
	}
	objects = append(objects, bootstrapConfigMapObject(marker))
	if err := checkDuplicates(objects); err != nil {
		return Set{}, err
	}
	return Set{Objects: objects, Secrets: slices.Clone(chart.Secrets)}, nil
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

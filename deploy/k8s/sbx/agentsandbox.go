// Package sbx renders the gt-sbx cluster's objects from pinned releases.
package sbx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

const (
	// AgentSandboxVersion is the agent-sandbox release the render pins, exactly,
	// so no caller can render a different one.
	AgentSandboxVersion = "v1.0.7"
	// AgentSandboxSHA256 is the sha256 of that release's sandbox.yaml, the digest
	// up.sh recorded on its first fetch. A release tag can be re-pointed at
	// different bytes; the digest is what pins them.
	AgentSandboxSHA256 = "437e6875cf7dfbd1c1230de3641f73bb9dad595a581366ef7f98f9b2813dd686"
)

const (
	// controllerDeployment is the Deployment up.sh patches after applying the
	// manifest; the pod runs on the core pool and Guaranteed (gt-cjm8fu).
	controllerDeployment = "agent-sandbox-controller"
	// controllerCPU and controllerMemory are the request and limit up.sh gives
	// every container and init container of that Deployment.
	controllerCPU    = "100m"
	controllerMemory = "128Mi"
	// coreNodeSelectorKey and coreNodeSelectorValue send the controller to the
	// core pool: the sandbox pool is tainted and carries the sandbox pods,
	// which may read only their own node's Secrets.
	coreNodeSelectorKey   = "gt.dev/pool"
	coreNodeSelectorValue = "core"
)

// agentSandboxObjectScopes maps every kind the pinned release may carry to
// whether it is cluster-scoped. A kind missing here is refused rather than read
// as namespaced: a namespaced document with no metadata.namespace would
// otherwise render into whatever namespace the applier's context holds.
var agentSandboxObjectScopes = map[string]bool{
	"Namespace":                true,
	"CustomResourceDefinition": true,
	"ClusterRole":              true,
	"ServiceAccount":           false,
	"Deployment":               false,
}

// AgentSandbox returns the pinned release's objects, read through read from
// path, with the two changes up.sh makes to the applied manifest: the core
// nodeSelector and requests equal to limits on every container and init
// container of the controller Deployment.
func AgentSandbox(read func(path string) ([]byte, error), path string) ([]k8s.StaticObject, error) {
	return agentSandbox(read, path, AgentSandboxSHA256, yaml.Marshal)
}

// agentSandbox is AgentSandbox against digest and marshal, so a test renders a
// fixture whose bytes are not the release's and drives the encoder's failure.
func agentSandbox(read func(path string) ([]byte, error), path, digest string, marshal func(any) ([]byte, error)) ([]k8s.StaticObject, error) {
	raw, err := read(path)
	if err != nil {
		return nil, fmt.Errorf("reading the agent-sandbox %s manifest %s: %w", AgentSandboxVersion, path, err)
	}
	sum := sha256.Sum256(raw)
	if actual := hex.EncodeToString(sum[:]); actual != digest {
		return nil, fmt.Errorf("agent-sandbox %s manifest %s is sha256 %s, want %s: refusing to render a manifest that is not the pinned release",
			AgentSandboxVersion, path, actual, digest)
	}
	objects, err := agentSandboxObjects(raw, marshal)
	if err != nil {
		return nil, fmt.Errorf("agent-sandbox %s manifest %s: %w", AgentSandboxVersion, path, err)
	}
	return objects, nil
}

// agentSandboxObjects parses the manifest's documents and returns them with the
// controller's patches applied.
func agentSandboxObjects(raw []byte, marshal func(any) ([]byte, error)) ([]k8s.StaticObject, error) {
	docs := splitDocuments(raw)
	if len(docs) == 0 {
		return nil, errors.New("holds no document")
	}
	objects := make([]k8s.StaticObject, 0, len(docs))
	nodes := make([]*yaml.Node, len(docs))
	controllers := []int{}
	for i, doc := range docs {
		node, err := documentNode(doc)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		object, err := readAgentSandboxObject(doc, node)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		if object.Kind == "Deployment" && object.Name == controllerDeployment {
			controllers = append(controllers, i)
		}
		nodes[i] = node
		objects = append(objects, object)
	}
	// An empty patch would otherwise report success with the controller left
	// unpatched and free to start on the tainted sandbox node.
	switch len(controllers) {
	case 0:
		return nil, fmt.Errorf("holds no Deployment named %s to patch", controllerDeployment)
	case 1:
	default:
		return nil, fmt.Errorf("holds %d Deployments named %s, want one", len(controllers), controllerDeployment)
	}
	index := controllers[0]
	if err := patchController(nodes[index]); err != nil {
		return nil, err
	}
	patched, err := marshal(nodes[index])
	if err != nil {
		return nil, fmt.Errorf("re-encoding Deployment %s: %w", controllerDeployment, err)
	}
	objects[index].Doc = bytes.TrimSpace(patched)
	return objects, nil
}

// documentNode decodes exactly one YAML mapping from doc, refusing a document
// that holds a second one between separators the splitter missed.
func documentNode(doc []byte) (*yaml.Node, error) {
	var node yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	if err := dec.Decode(&node); err != nil {
		return nil, fmt.Errorf("not a YAML object: %w", err)
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, fmt.Errorf("second document: %w", err)
	default:
		return nil, errors.New("more than one YAML document between separators")
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("not a YAML mapping")
	}
	return &node, nil
}

// readAgentSandboxObject reads one document's object and checks the fields the
// render needs to place it.
func readAgentSandboxObject(doc []byte, node *yaml.Node) (k8s.StaticObject, error) {
	root := node.Content[0]
	apiVersion := scalarValue(root, "apiVersion")
	kind := scalarValue(root, "kind")
	metadata := mappingValue(root, "metadata")
	name := scalarValue(metadata, "name")
	namespace := scalarValue(metadata, "namespace")
	switch {
	case apiVersion == "":
		return k8s.StaticObject{}, errors.New("no apiVersion")
	case kind == "":
		return k8s.StaticObject{}, errors.New("no kind")
	case name == "":
		return k8s.StaticObject{}, errors.New("no metadata.name")
	}
	cluster, known := agentSandboxObjectScopes[kind]
	if !known {
		return k8s.StaticObject{}, fmt.Errorf("kind %s is not a kind the agent-sandbox %s release carries", kind, AgentSandboxVersion)
	}
	if !cluster && namespace == "" {
		return k8s.StaticObject{}, fmt.Errorf("namespaced %s %s has no metadata.namespace", kind, name)
	}
	object := k8s.StaticObject{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		Namespace:  namespace,
		Cluster:    cluster,
		Doc:        doc,
	}
	if cluster {
		object.Namespace = ""
	}
	return object, nil
}

// patchController applies up.sh's two changes to the controller Deployment's
// document: the core nodeSelector on the pod template, and requests equal to
// limits on every container and init container.
func patchController(node *yaml.Node) error {
	podSpec, err := mappingPath(node.Content[0], "spec", "template", "spec")
	if err != nil {
		return fmt.Errorf("Deployment %s: %w", controllerDeployment, err)
	}
	selector := mappingValue(podSpec, "nodeSelector")
	switch {
	case selector == nil:
		selector = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMappingNode(podSpec, "nodeSelector", selector)
	case selector.Kind != yaml.MappingNode:
		return fmt.Errorf("Deployment %s: spec.template.spec.nodeSelector is not a mapping", controllerDeployment)
	}
	setMappingNode(selector, coreNodeSelectorKey, scalarNode(coreNodeSelectorValue))

	if err := patchContainers(podSpec, "containers", true); err != nil {
		return fmt.Errorf("Deployment %s: %w", controllerDeployment, err)
	}
	if err := patchContainers(podSpec, "initContainers", false); err != nil {
		return fmt.Errorf("Deployment %s: %w", controllerDeployment, err)
	}
	return nil
}

// patchContainers sizes every container in the pod spec's sequence under key.
// A pod with no containers is refused when required, because sizing nothing
// would otherwise report success.
func patchContainers(podSpec *yaml.Node, key string, required bool) error {
	containers := mappingValue(podSpec, key)
	if containers == nil {
		if required {
			return fmt.Errorf("spec.template.spec.%s is missing, so the pod would not run Guaranteed", key)
		}
		return nil
	}
	if containers.Kind != yaml.SequenceNode {
		return fmt.Errorf("spec.template.spec.%s is not a sequence", key)
	}
	if required && len(containers.Content) == 0 {
		return fmt.Errorf("spec.template.spec.%s is empty, so the pod would not run Guaranteed", key)
	}
	for _, container := range containers.Content {
		if container.Kind != yaml.MappingNode {
			return fmt.Errorf("spec.template.spec.%s holds an item that is not a mapping", key)
		}
		setGuaranteedResources(container)
	}
	return nil
}

// setGuaranteedResources replaces container's resources with requests equal to
// limits of the controller's pair. up.sh's JSON patch "add" replaces the whole
// field, and a container keeping an extra request would fall out of the
// Guaranteed class (gt-cjm8fu).
func setGuaranteedResources(container *yaml.Node) {
	resources := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, field := range []string{"requests", "limits"} {
		resources.Content = append(resources.Content,
			scalarNode(field),
			&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
				scalarNode("cpu"), scalarNode(controllerCPU),
				scalarNode("memory"), scalarNode(controllerMemory),
			}})
	}
	setMappingNode(container, "resources", resources)
}

// splitDocuments splits one manifest file into its documents on separator lines
// and drops the documents that hold only blank space.
func splitDocuments(raw []byte) [][]byte {
	var docs [][]byte
	var cur []byte
	flush := func() {
		if doc := bytes.TrimSpace(cur); len(doc) > 0 {
			docs = append(docs, doc)
		}
		cur = nil
	}
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		if strings.TrimRight(line, " \t\r\n") == "---" {
			flush()
			continue
		}
		cur = append(cur, line...)
	}
	flush()
	return docs
}

// mappingPath walks the mapping keys from m, refusing a key that is missing or
// whose value is not a mapping.
func mappingPath(m *yaml.Node, keys ...string) (*yaml.Node, error) {
	current := m
	for i, key := range keys {
		next := mappingValue(current, key)
		if next == nil {
			return nil, fmt.Errorf("no %s", strings.Join(keys[:i+1], "."))
		}
		if next.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s is not a mapping", strings.Join(keys[:i+1], "."))
		}
		current = next
	}
	return current, nil
}

// mappingValue returns the value node under key in m, or nil when m is not a
// mapping or holds no key.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// scalarValue returns the string under key in m, or "" when there is none.
func scalarValue(m *yaml.Node, key string) string {
	value := mappingValue(m, key)
	if value == nil || value.Kind != yaml.ScalarNode {
		return ""
	}
	return value.Value
}

// setMappingNode sets key to value in m, replacing the node already there.
func setMappingNode(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, scalarNode(key), value)
}

func scalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

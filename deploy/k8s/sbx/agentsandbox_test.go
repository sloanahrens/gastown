package sbx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const fixturePath = "testdata/agent-sandbox-trimmed.yaml"

// containerDoc is the part of a controller container the patches decide.
type containerDoc struct {
	Name      string `yaml:"name"`
	Resources struct {
		Requests map[string]string `yaml:"requests"`
		Limits   map[string]string `yaml:"limits"`
	} `yaml:"resources"`
}

// deploymentDoc is the part of the controller Deployment the patches decide.
type deploymentDoc struct {
	Spec struct {
		Template struct {
			Spec struct {
				NodeSelector   map[string]string `yaml:"nodeSelector"`
				Containers     []containerDoc    `yaml:"containers"`
				InitContainers []containerDoc    `yaml:"initContainers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func readFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("reading %s: %v", fixturePath, err)
	}
	return raw
}

// digestOf is the digest the fixture is rendered against: the release's own
// digest pin cannot match a trimmed file.
func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func readerOf(raw []byte) func(path string) ([]byte, error) {
	return func(string) ([]byte, error) { return raw, nil }
}

// The exported call site checks the release's pin, so any bytes that are not the
// pinned release are refused with both digests and nothing returned.
func TestAgentSandboxRefusesADifferentDigest(t *testing.T) {
	t.Parallel()
	raw := readFixture(t)
	objects, err := AgentSandbox(readerOf(raw), fixturePath)
	if err == nil {
		t.Fatalf("AgentSandbox(fixture) = %d objects, nil error; want the digest refusal", len(objects))
	}
	if objects != nil {
		t.Errorf("AgentSandbox(fixture) returned %d objects beside the error", len(objects))
	}
	for _, digest := range []string{digestOf(raw), AgentSandboxSHA256} {
		if !strings.Contains(err.Error(), digest) {
			t.Errorf("error %q does not name digest %s", err, digest)
		}
	}
}

func TestAgentSandboxPatchesController(t *testing.T) {
	t.Parallel()
	raw := readFixture(t)
	objects, err := agentSandbox(readerOf(raw), fixturePath, digestOf(raw), yaml.Marshal)
	if err != nil {
		t.Fatalf("agentSandbox(fixture): %v", err)
	}
	wants := []struct {
		kind, name, namespace string
		cluster               bool
	}{
		{"Namespace", "agent-sandbox-system", "", true},
		{"CustomResourceDefinition", "sandboxes.agents.x-k8s.io", "", true},
		{"ClusterRole", "agent-sandbox-controller", "", true},
		{"ServiceAccount", "agent-sandbox-controller", "agent-sandbox-system", false},
		{"Deployment", "agent-sandbox-controller", "agent-sandbox-system", false},
	}
	if len(objects) != len(wants) {
		t.Fatalf("agentSandbox(fixture) returned %d objects, want %d", len(objects), len(wants))
	}
	for i, want := range wants {
		got := objects[i]
		if got.Kind != want.kind || got.Name != want.name || got.Namespace != want.namespace || got.Cluster != want.cluster {
			t.Errorf("object %d = %s %s (namespace %q, cluster %v), want %s %s (namespace %q, cluster %v)",
				i, got.Kind, got.Name, got.Namespace, got.Cluster, want.kind, want.name, want.namespace, want.cluster)
		}
	}

	t.Run("no other document is rewritten", func(t *testing.T) {
		t.Parallel()
		docs := splitDocuments(raw)
		for i, object := range objects {
			if object.Kind == "Deployment" {
				continue
			}
			if !bytes.Equal(object.Doc, docs[i]) {
				t.Errorf("document %d (%s %s) was rewritten", i, object.Kind, object.Name)
			}
		}
	})

	t.Run("the controller runs on the core pool, Guaranteed", func(t *testing.T) {
		t.Parallel()
		var deployment deploymentDoc
		if err := yaml.Unmarshal(objects[4].Doc, &deployment); err != nil {
			t.Fatalf("parsing the patched Deployment: %v", err)
		}
		podSpec := deployment.Spec.Template.Spec
		if want := map[string]string{coreNodeSelectorKey: coreNodeSelectorValue}; !maps.Equal(podSpec.NodeSelector, want) {
			t.Errorf("nodeSelector = %v, want %v", podSpec.NodeSelector, want)
		}
		if len(podSpec.Containers) != 2 {
			t.Fatalf("containers = %d, want the fixture's 2", len(podSpec.Containers))
		}
		if len(podSpec.InitContainers) != 1 {
			t.Fatalf("initContainers = %d, want the fixture's 1", len(podSpec.InitContainers))
		}
		want := map[string]string{"cpu": controllerCPU, "memory": controllerMemory}
		for _, container := range slices.Concat(podSpec.Containers, podSpec.InitContainers) {
			if !maps.Equal(container.Resources.Requests, want) {
				t.Errorf("container %s requests = %v, want %v", container.Name, container.Resources.Requests, want)
			}
			if !maps.Equal(container.Resources.Limits, want) {
				t.Errorf("container %s limits = %v, want %v", container.Name, container.Resources.Limits, want)
			}
		}
	})
}

func TestAgentSandboxRequiresTheControllerDeployment(t *testing.T) {
	t.Parallel()
	raw := []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: agent-sandbox-system\n")
	objects, err := agentSandbox(readerOf(raw), "manifest.yaml", digestOf(raw), yaml.Marshal)
	if err == nil || !strings.Contains(err.Error(), controllerDeployment) {
		t.Fatalf("agentSandbox(no controller) = %d objects, %v; want a refusal naming %s", len(objects), err, controllerDeployment)
	}
}

func TestAgentSandboxReadsOnlyItsPath(t *testing.T) {
	t.Parallel()
	raw := readFixture(t)
	const path = "deploy/cache/agent-sandbox-v1.0.7.yaml"
	var reads []string
	read := func(p string) ([]byte, error) {
		reads = append(reads, p)
		return raw, nil
	}
	if _, err := agentSandbox(read, path, digestOf(raw), yaml.Marshal); err != nil {
		t.Fatalf("agentSandbox(): %v", err)
	}
	if !slices.Equal(reads, []string{path}) {
		t.Fatalf("the reader was called with %q, want only %q", reads, path)
	}
}

func TestAgentSandboxReaderFailure(t *testing.T) {
	t.Parallel()
	read := func(path string) ([]byte, error) { return nil, errors.New("planted failure") }
	_, err := agentSandbox(read, "manifest.yaml", digestOf(nil), yaml.Marshal)
	if err == nil || !strings.Contains(err.Error(), "planted failure") {
		t.Fatalf("agentSandbox() = %v, want the reader's error", err)
	}
	if !strings.Contains(err.Error(), "manifest.yaml") {
		t.Errorf("error %q does not name the path it read", err)
	}
}

// controllerManifest is a manifest holding only the controller Deployment, with
// podSpec as its spec.template.spec, so a row changes only what it is about.
func controllerManifest(podSpec string) []byte {
	return []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: agent-sandbox-controller
  namespace: agent-sandbox-system
spec:
  template:
    spec:
` + podSpec)
}

// Every document the render cannot place, and every Deployment it cannot size,
// is refused with nothing returned.
func TestAgentSandboxRefusals(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name     string
		manifest []byte
		want     string
	}{
		{"an empty manifest", nil, "holds no document"},
		{"a document with no apiVersion", []byte("kind: Namespace\nmetadata:\n  name: x\n"), "no apiVersion"},
		{"a document with no kind", []byte("apiVersion: v1\nmetadata:\n  name: x\n"), "no kind"},
		{"a document with no metadata.name", []byte("apiVersion: v1\nkind: Namespace\nmetadata: {}\n"), "no metadata.name"},
		{"a document whose metadata is not a mapping", []byte("apiVersion: v1\nkind: Namespace\nmetadata: x\n"), "no metadata.name"},
		{"a kind the release does not carry", []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"), "kind ConfigMap"},
		{"a namespaced kind with no namespace", []byte("apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: x\n"), "metadata.namespace"},
		{"a document that is not a mapping", []byte("- one\n- two\n"), "not a YAML mapping"},
		{"a document that is not YAML", []byte("apiVersion: [\n"), "not a YAML object"},
		{"a separator the splitter missed", []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: a\n--- # second\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: b\n"), "more than one YAML document"},
		{"a second document that is not YAML", []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: a\n--- # second\nkind: [\n"), "second document"},
		{"a Deployment with no spec.template", []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: agent-sandbox-controller\n  namespace: agent-sandbox-system\nspec: {}\n"), "no spec.template"},
		{"a Deployment whose spec is not a mapping", []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: agent-sandbox-controller\n  namespace: agent-sandbox-system\nspec: not-a-mapping\n"), "spec is not a mapping"},
		{"a controller whose nodeSelector is not a mapping", controllerManifest("      nodeSelector: core\n      containers:\n        - name: c\n"), "nodeSelector is not a mapping"},
		{"a controller with no containers", controllerManifest("      serviceAccountName: agent-sandbox-controller\n"), "containers is missing"},
		{"a controller with an empty container list", controllerManifest("      containers: []\n"), "containers is empty"},
		{"a controller whose containers are not a sequence", controllerManifest("      containers: {}\n"), "containers is not a sequence"},
		{"a controller with a container that is not a mapping", controllerManifest("      containers:\n        - just-a-string\n"), "not a mapping"},
		{"a controller whose init containers are not a sequence", controllerManifest("      containers:\n        - name: c\n      initContainers: {}\n"), "initContainers is not a sequence"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			objects, err := agentSandbox(readerOf(row.manifest), "manifest.yaml", digestOf(row.manifest), yaml.Marshal)
			if err == nil {
				t.Fatalf("agentSandbox() = %d objects, nil error; want a refusal naming %s", len(objects), row.want)
			}
			if objects != nil {
				t.Errorf("agentSandbox() returned %d objects beside the error", len(objects))
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("error %q does not name %s", err, row.want)
			}
		})
	}
}

// A release with no init containers is sized like any other: every container
// gets the pair, and the missing init container list is not an error.
func TestAgentSandboxSizesAPodWithNoInitContainers(t *testing.T) {
	t.Parallel()
	raw := controllerManifest("      containers:\n        - name: c\n")
	objects, err := agentSandbox(readerOf(raw), "manifest.yaml", digestOf(raw), yaml.Marshal)
	if err != nil {
		t.Fatalf("agentSandbox(pod with no init containers): %v", err)
	}
	var deployment deploymentDoc
	if err := yaml.Unmarshal(objects[0].Doc, &deployment); err != nil {
		t.Fatalf("parsing the patched Deployment: %v", err)
	}
	want := map[string]string{"cpu": controllerCPU, "memory": controllerMemory}
	if !maps.Equal(deployment.Spec.Template.Spec.Containers[0].Resources.Requests, want) {
		t.Errorf("requests = %v, want %v", deployment.Spec.Template.Spec.Containers[0].Resources.Requests, want)
	}
}

func TestAgentSandboxRefusesAnUnencodableManifest(t *testing.T) {
	t.Parallel()
	raw := readFixture(t)
	marshal := func(any) ([]byte, error) { return nil, errors.New("planted failure") }
	objects, err := agentSandbox(readerOf(raw), "manifest.yaml", digestOf(raw), marshal)
	if err == nil || !strings.Contains(err.Error(), "planted failure") {
		t.Fatalf("agentSandbox() = %d objects, %v; want the encoder's error", len(objects), err)
	}
	if objects != nil {
		t.Errorf("agentSandbox() returned %d objects beside the error", len(objects))
	}
	if !strings.Contains(err.Error(), "re-encoding") {
		t.Errorf("error %q does not say which step failed", err)
	}
}

func TestAgentSandboxRefusesTwoControllers(t *testing.T) {
	t.Parallel()
	raw := append(controllerManifest("      containers:\n        - name: c\n"), "---\n"...)
	raw = append(raw, controllerManifest("      containers:\n        - name: c\n")...)
	_, err := agentSandbox(readerOf(raw), "manifest.yaml", digestOf(raw), yaml.Marshal)
	if err == nil || !strings.Contains(err.Error(), "want one") {
		t.Fatalf("agentSandbox(two controllers) = %v, want a refusal naming the count", err)
	}
}

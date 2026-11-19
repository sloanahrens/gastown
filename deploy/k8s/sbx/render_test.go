package sbx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

const (
	renderChartDir   = "deploy/cache/openshell"
	renderValuesPath = "deploy/k8s/values/kind.yaml"
)

// renderValues is the values file every render test hashes into the marker.
var renderValues = []byte("agentSandbox:\n  preflight:\n    enabled: true\n")

// renderInputs is Inputs over the two trimmed fixtures and renderValues, with
// the runner's calls recorded into calls.
func renderInputs(sandbox, chartRaw []byte, calls *[]runnerCall) Inputs {
	return Inputs{
		Read: func(path string) ([]byte, error) {
			switch path {
			case fixturePath:
				return sandbox, nil
			case renderValuesPath:
				return renderValues, nil
			}
			return nil, fmt.Errorf("unexpected read of %s", path)
		},
		Run:              outputRunner(calls, chartRaw, nil),
		AgentSandboxPath: fixturePath,
		ChartDir:         renderChartDir,
		ValuesPath:       renderValuesPath,
	}
}

// joinDocs is the ordered list as one byte stream, so two renders compare byte
// for byte.
func joinDocs(objects []k8s.StaticObject) []byte {
	var out []byte
	for _, object := range objects {
		out = append(out, object.Doc...)
		out = append(out, "\n---\n"...)
	}
	return out
}

func objectKeys(objects []k8s.StaticObject) []string {
	keys := make([]string, len(objects))
	for i, object := range objects {
		keys[i] = objectKey(object)
	}
	return keys
}

// The composed set places every object in the apply order, from the namespaces
// through the bootstrap marker.
func TestSbxRenderOrder(t *testing.T) {
	t.Parallel()
	sandbox := readFixture(t)
	var calls []runnerCall
	set, err := renderSet(context.Background(), renderInputs(sandbox, readChartFixture(t), &calls), k8s.SbxStatic, digestOf(sandbox))
	if err != nil {
		t.Fatalf("renderSet(): %v", err)
	}
	want := []string{
		"Namespace/agent-sandbox-system",
		"Namespace/openshell",
		"PriorityClass/gt-control-plane",
		"PriorityClass/gt-sandbox",
		"PriorityClass/gt-ci-gate",
		"PriorityClass/gt-ci",
		"CustomResourceDefinition/sandboxes.agents.x-k8s.io",
		"ClusterRole/agent-sandbox-controller",
		"ServiceAccount/agent-sandbox-system/agent-sandbox-controller",
		"Deployment/agent-sandbox-system/agent-sandbox-controller",
		"StatefulSet/openshell/openshell",
		"Service/openshell/openshell",
		"Service/openshell/openshell-nodeport",
		"Service/openshell/control-plane",
		"Service/openshell/dolt",
		"Service/openshell/forgejo",
		"Service/openshell/tracker",
		"PersistentVolumeClaim/openshell/gt-cache",
		"ConfigMap/openshell/gt-bootstrap",
	}
	if got := objectKeys(set.Objects); !slices.Equal(got, want) {
		t.Errorf("Objects = %q,\nwant %q", got, want)
	}
}

// The openshell Namespace is built with the label the live one carries.
func TestSbxRenderOpenshellNamespace(t *testing.T) {
	t.Parallel()
	sandbox := readFixture(t)
	var calls []runnerCall
	set, err := renderSet(context.Background(), renderInputs(sandbox, readChartFixture(t), &calls), k8s.SbxStatic, digestOf(sandbox))
	if err != nil {
		t.Fatalf("renderSet(): %v", err)
	}
	var namespace struct {
		Metadata struct {
			Labels map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
	}
	object, ok := findStatic(set.Objects, "Namespace", openshellName)
	if !ok {
		t.Fatalf("Objects holds no Namespace %s", openshellName)
	}
	if err := yaml.Unmarshal(object.Doc, &namespace); err != nil {
		t.Fatalf("parsing the openshell Namespace: %v", err)
	}
	if namespace.Metadata.Labels["name"] != openshellName {
		t.Errorf("the openshell Namespace labels = %v, want name=%s", namespace.Metadata.Labels, openshellName)
	}
}

// Two renders of the same inputs are byte-identical.
func TestSbxRenderDeterministic(t *testing.T) {
	t.Parallel()
	sandbox := readFixture(t)
	chartRaw := readChartFixture(t)
	var firstCalls, secondCalls []runnerCall
	first, err := renderSet(context.Background(), renderInputs(sandbox, chartRaw, &firstCalls), k8s.SbxStatic, digestOf(sandbox))
	if err != nil {
		t.Fatalf("first renderSet(): %v", err)
	}
	second, err := renderSet(context.Background(), renderInputs(sandbox, chartRaw, &secondCalls), k8s.SbxStatic, digestOf(sandbox))
	if err != nil {
		t.Fatalf("second renderSet(): %v", err)
	}
	if len(first.Objects) != len(second.Objects) {
		t.Fatalf("the renders hold %d and %d objects", len(first.Objects), len(second.Objects))
	}
	if !bytes.Equal(joinDocs(first.Objects), joinDocs(second.Objects)) {
		t.Errorf("two renders of the same inputs differ")
	}
	if !slices.Equal(first.Secrets, second.Secrets) {
		t.Errorf("Secrets = %q and %q", first.Secrets, second.Secrets)
	}
}

// A second object with the same kind, namespace and name is refused, naming it.
func TestSbxRenderRefusesADuplicate(t *testing.T) {
	t.Parallel()
	sandbox := readFixture(t)
	// The chart output carrying its own gt-bootstrap ConfigMap collides with
	// the marker the render writes.
	chartRaw := chartOf(string(readChartFixture(t)), `apiVersion: v1
kind: ConfigMap
metadata:
  name: gt-bootstrap
  namespace: openshell
data:
  helm: "0.1.4:planted"
`)
	var calls []runnerCall
	set, err := renderSet(context.Background(), renderInputs(sandbox, chartRaw, &calls), k8s.SbxStatic, digestOf(sandbox))
	if err == nil {
		t.Fatalf("renderSet() = %d objects, nil error; want the duplicate refusal", len(set.Objects))
	}
	if want := "ConfigMap/openshell/gt-bootstrap"; !strings.Contains(err.Error(), "duplicate object "+want) {
		t.Errorf("error %q does not name the duplicate %s", err, want)
	}
	if len(set.Objects) != 0 || set.Secrets != nil {
		t.Errorf("renderSet() returned %+v beside the error", set)
	}
}

// The marker carries the chart version and the values file's digest, no Secret
// is placed, and Secrets names the chart's Secrets and nothing else.
func TestSbxRenderBootstrapMarker(t *testing.T) {
	t.Parallel()
	sandbox := readFixture(t)
	var calls []runnerCall
	set, err := renderSet(context.Background(), renderInputs(sandbox, readChartFixture(t), &calls), k8s.SbxStatic, digestOf(sandbox))
	if err != nil {
		t.Fatalf("renderSet(): %v", err)
	}
	object, ok := findStatic(set.Objects, "ConfigMap", bootstrapConfigMap)
	if !ok {
		t.Fatalf("Objects holds no ConfigMap %s", bootstrapConfigMap)
	}
	var configMap struct {
		Metadata struct {
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(object.Doc, &configMap); err != nil {
		t.Fatalf("parsing %s: %v", bootstrapConfigMap, err)
	}
	if want := openshellName; configMap.Metadata.Namespace != want {
		t.Errorf("%s namespace = %q, want %q", bootstrapConfigMap, configMap.Metadata.Namespace, want)
	}
	if want := ChartVersion + ":" + digestOf(renderValues); configMap.Data[bootstrapMarkerKey] != want {
		t.Errorf("%s data.%s = %q, want %q", bootstrapConfigMap, bootstrapMarkerKey, configMap.Data[bootstrapMarkerKey], want)
	}
	for _, object := range set.Objects {
		if object.Kind == "Secret" {
			t.Errorf("Objects holds the Secret %s", object.Name)
		}
	}
	wantSecrets := []string{
		"openshell-credential-storage-key-encryption-key",
		"openshell-server-tls",
		"openshell-client-tls",
		"openshell-jwt-keys",
	}
	if !slices.Equal(set.Secrets, wantSecrets) {
		t.Errorf("Secrets = %q, want %q", set.Secrets, wantSecrets)
	}
}

// Render itself checks the agent-sandbox bytes against the release's pin.
func TestSbxRenderChecksThePinnedManifest(t *testing.T) {
	t.Parallel()
	sandbox := readFixture(t)
	var calls []runnerCall
	set, err := Render(context.Background(), renderInputs(sandbox, readChartFixture(t), &calls))
	if err == nil {
		t.Fatalf("Render(fixture) = %d objects, nil error; want the pin refusal", len(set.Objects))
	}
	if !strings.Contains(err.Error(), AgentSandboxSHA256) {
		t.Errorf("error %q does not name the pin %s", err, AgentSandboxSHA256)
	}
	if len(set.Objects) != 0 || set.Secrets != nil {
		t.Errorf("Render() returned %+v beside the error", set)
	}
}

// hasRef reports whether refs holds the named object.
func hasRef(refs []ObjectRef, kind, namespace, name string) bool {
	for _, ref := range refs {
		if ref.Kind == kind && ref.Namespace == namespace && ref.Name == name {
			return true
		}
	}
	return false
}

// Runtime names a member of each class of object the sandbox cluster creates
// itself, and not a lookalike of a bootstrap ClusterRole.
func TestSbxRuntimeNamesTheClusterObjects(t *testing.T) {
	t.Parallel()
	sandbox := readFixture(t)
	var calls []runnerCall
	set, err := renderSet(context.Background(), renderInputs(sandbox, readChartFixture(t), &calls), k8s.SbxStatic, digestOf(sandbox))
	if err != nil {
		t.Fatalf("renderSet(): %v", err)
	}
	// The gateway and cache-trim objects.
	for _, ref := range []ObjectRef{
		{Kind: "NetworkPolicy", Namespace: openshellName, Name: "openshell-sandbox-supervisors"},
		{Kind: "Job", Namespace: openshellName, Name: "gt-cache-trim-gt-cache"},
	} {
		if !hasRef(set.Runtime, ref.Kind, ref.Namespace, ref.Name) {
			t.Errorf("Runtime holds no %+v", ref)
		}
	}
	// The five namespaces.
	for _, name := range []string{"default", "kube-system", "kube-public", "kube-node-lease", "local-path-storage"} {
		if !hasRef(set.Runtime, "Namespace", "", name) {
			t.Errorf("Runtime holds no Namespace %s", name)
		}
	}
	// The local-path storage objects.
	if !hasRef(set.Runtime, "Deployment", "local-path-storage", "local-path-provisioner") {
		t.Errorf("Runtime holds no local-path provisioner")
	}
	// The fixed-name cluster-scoped objects.
	if !hasRef(set.Runtime, "ClusterRole", "", "admin") {
		t.Errorf("Runtime holds no ClusterRole admin")
	}
	if !hasRef(set.Runtime, "ClusterRoleBinding", "", "cluster-admin") {
		t.Errorf("Runtime holds no ClusterRoleBinding cluster-admin")
	}
	if hasRef(set.Runtime, "ClusterRole", "", "admin-reader") {
		t.Errorf("Runtime holds the lookalike ClusterRole admin-reader")
	}
}

// Runtime and the rendered Objects never name the same object.
func TestSbxRuntimeDoesNotOverlapObjects(t *testing.T) {
	t.Parallel()
	sandbox := readFixture(t)
	var calls []runnerCall
	set, err := renderSet(context.Background(), renderInputs(sandbox, readChartFixture(t), &calls), k8s.SbxStatic, digestOf(sandbox))
	if err != nil {
		t.Fatalf("renderSet(): %v", err)
	}
	rendered := make(map[string]bool, len(set.Objects))
	for _, object := range set.Objects {
		rendered[objectKey(object)] = true
	}
	for _, ref := range set.Runtime {
		key := objectKey(k8s.StaticObject{Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name})
		if rendered[key] {
			t.Errorf("Runtime names the rendered object %s", key)
		}
	}
}

// A caller that changes the Runtime it got does not change the next render.
func TestSbxRuntimeIsACopy(t *testing.T) {
	t.Parallel()
	sandbox := readFixture(t)
	chartRaw := readChartFixture(t)
	var firstCalls, secondCalls []runnerCall
	first, err := renderSet(context.Background(), renderInputs(sandbox, chartRaw, &firstCalls), k8s.SbxStatic, digestOf(sandbox))
	if err != nil {
		t.Fatalf("first renderSet(): %v", err)
	}
	before := slices.Clone(first.Runtime)
	if len(before) == 0 {
		t.Fatal("Runtime is empty")
	}
	first.Runtime[0] = ObjectRef{Kind: "ClusterRole", Name: "planted"}
	second, err := renderSet(context.Background(), renderInputs(sandbox, chartRaw, &secondCalls), k8s.SbxStatic, digestOf(sandbox))
	if err != nil {
		t.Fatalf("second renderSet(): %v", err)
	}
	if !slices.Equal(second.Runtime, before) {
		t.Errorf("a change to one render's Runtime changed the next render's Runtime")
	}
}

// Every input the render cannot read, and every static set it cannot place, is
// refused with no set returned.
func TestSbxRenderRefusals(t *testing.T) {
	t.Parallel()
	sandbox := readFixture(t)
	chartRaw := readChartFixture(t)
	static, err := k8s.SbxStatic()
	if err != nil {
		t.Fatalf("k8s.SbxStatic(): %v", err)
	}
	without := func(kind, name string) func() ([]k8s.StaticObject, error) {
		return func() ([]k8s.StaticObject, error) {
			var kept []k8s.StaticObject
			for _, object := range static {
				if object.Kind == kind && object.Name == name {
					continue
				}
				kept = append(kept, object)
			}
			return kept, nil
		}
	}
	rows := []struct {
		name   string
		in     func(Inputs) Inputs
		static func() ([]k8s.StaticObject, error)
		digest string
		want   []string
	}{
		{
			name: "no reader",
			in:   func(in Inputs) Inputs { in.Read = nil; return in },
			want: []string{"no reader"},
		},
		{
			name: "a values file that cannot be read",
			in: func(in Inputs) Inputs {
				base := in.Read
				in.Read = func(path string) ([]byte, error) {
					if path == renderValuesPath {
						return nil, errors.New("planted failure")
					}
					return base(path)
				}
				return in
			},
			want: []string{"reading the values file " + renderValuesPath, "planted failure"},
		},
		{
			name: "an agent-sandbox manifest that cannot be read",
			in: func(in Inputs) Inputs {
				base := in.Read
				in.Read = func(path string) ([]byte, error) {
					if path == fixturePath {
						return nil, errors.New("planted failure")
					}
					return base(path)
				}
				return in
			},
			want: []string{fixturePath, "planted failure"},
		},
		{
			name:   "an agent-sandbox manifest that is not the pinned release",
			digest: AgentSandboxSHA256,
			want:   []string{"not the pinned release"},
		},
		{
			name: "no runner",
			in:   func(in Inputs) Inputs { in.Run = nil; return in },
			want: []string{"no runner"},
		},
		{
			name: "a static source that fails",
			static: func() ([]k8s.StaticObject, error) {
				return nil, errors.New("planted failure")
			},
			want: []string{"static set", "planted failure"},
		},
		{
			name:   "a static set without the gateway NodePort",
			static: without("Service", "openshell-nodeport"),
			want:   []string{"no Service openshell-nodeport"},
		},
		{
			name: "a static object the order does not place",
			static: func() ([]k8s.StaticObject, error) {
				return append(slices.Clone(static), k8s.StaticObject{
					APIVersion: "v1", Kind: "Service", Name: "surprise", Namespace: openshellName,
					Doc: []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: surprise\n  namespace: openshell\n"),
				}), nil
			},
			want: []string{"does not place", "Service surprise"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			in := renderInputs(sandbox, chartRaw, &[]runnerCall{})
			if row.in != nil {
				in = row.in(in)
			}
			staticSource := k8s.SbxStatic
			if row.static != nil {
				staticSource = row.static
			}
			digest := digestOf(sandbox)
			if row.digest != "" {
				digest = row.digest
			}
			set, err := renderSet(context.Background(), in, staticSource, digest)
			if err == nil {
				t.Fatalf("renderSet() = %d objects, nil error; want a refusal naming %q", len(set.Objects), row.want)
			}
			if len(set.Objects) != 0 || set.Secrets != nil {
				t.Errorf("renderSet() returned %+v beside the error", set)
			}
			for _, want := range row.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

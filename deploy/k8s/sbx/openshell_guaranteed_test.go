package sbx

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// gatewayPod is the smallest pod the gateway can run: one container, no init
// container.
const gatewayPod = "      containers:\n        - name: openshell\n"

// gatewayStatefulSetDoc is the gateway StatefulSet with podSpec as its
// spec.template.spec, so a row changes only the pod.
func gatewayStatefulSetDoc(podSpec string) string {
	return `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: openshell
  namespace: openshell
spec:
  serviceName: openshell
  template:
    spec:
` + podSpec
}

// certgenPodSpec names the three Secrets the certgen hook Job creates, so a
// chart reaches the sizing step past the Secret naming check.
const certgenPodSpec = `      containers:
        - name: certgen
          args:
            - --server-secret-name=openshell-server-tls
            - --client-secret-name=openshell-client-tls
            - --jwt-secret-name=openshell-jwt-keys
`

// chartWith joins the certgen hook Job and the given documents into one helm
// output.
func chartWith(docs ...string) string {
	return string(chartOf(append([]string{certgenJobDoc(certgenPodSpec)}, docs...)...))
}

// statefulSetDoc is the part of the gateway StatefulSet the sizing decides.
type statefulSetDoc struct {
	Spec struct {
		Template struct {
			Spec struct {
				Containers     []containerDoc `yaml:"containers"`
				InitContainers []containerDoc `yaml:"initContainers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// fixtureDoc returns the fixture's document of the given kind.
func fixtureDoc(t *testing.T, kind string) []byte {
	t.Helper()
	for _, doc := range splitDocuments(readChartFixture(t)) {
		var header struct {
			Kind string `yaml:"kind"`
		}
		if err := yaml.Unmarshal(doc, &header); err != nil {
			t.Fatalf("parsing a fixture document: %v", err)
		}
		if header.Kind == kind {
			return doc
		}
	}
	t.Fatalf("the fixture holds no %s", kind)
	return nil
}

func decodeDoc(t *testing.T, doc []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := yaml.Unmarshal(doc, &body); err != nil {
		t.Fatalf("parsing the document: %v", err)
	}
	return body
}

// stripResources deletes every container's resources from the pod spec at
// spec.template.spec, failing the test when the document holds none.
func stripResources(t *testing.T, body map[string]any) {
	t.Helper()
	current := body
	for _, key := range []string{"spec", "template", "spec"} {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("the document holds no %s mapping", key)
		}
		current = next
	}
	for _, key := range []string{"containers", "initContainers"} {
		list, ok := current[key]
		if !ok {
			continue
		}
		items, ok := list.([]any)
		if !ok {
			t.Fatalf("%s is not a list", key)
		}
		for _, item := range items {
			container, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("a %s item is not a mapping", key)
			}
			delete(container, "resources")
		}
	}
}

// The gateway StatefulSet runs Guaranteed: every container and init container
// carries requests equal to limits of the gateway's pair, and the sizing
// rewrites nothing else in any object.
func TestOpenShellGuaranteed(t *testing.T) {
	t.Parallel()
	raw := readChartFixture(t)
	var calls []runnerCall
	chart, err := OpenShell(context.Background(), outputRunner(&calls, raw, nil), "chart", "values.yaml")
	if err != nil {
		t.Fatalf("OpenShell(fixture): %v", err)
	}
	index := -1
	for i, object := range chart.Objects {
		if object.Kind == "StatefulSet" && object.Name == gatewayStatefulSet {
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("Objects = %+v, want the gateway StatefulSet", chart.Objects)
	}

	t.Run("every container is Guaranteed", func(t *testing.T) {
		t.Parallel()
		var gateway statefulSetDoc
		if err := yaml.Unmarshal(chart.Objects[index].Doc, &gateway); err != nil {
			t.Fatalf("parsing the patched StatefulSet: %v", err)
		}
		podSpec := gateway.Spec.Template.Spec
		if len(podSpec.Containers) == 0 {
			t.Fatal("the fixture's gateway has no container, so the sizing is not exercised")
		}
		want := map[string]string{"cpu": gatewayCPU, "memory": gatewayMemory}
		for _, container := range slices.Concat(podSpec.Containers, podSpec.InitContainers) {
			if !maps.Equal(container.Resources.Requests, want) {
				t.Errorf("container %s requests = %v, want %v", container.Name, container.Resources.Requests, want)
			}
			if !maps.Equal(container.Resources.Limits, want) {
				t.Errorf("container %s limits = %v, want %v", container.Name, container.Resources.Limits, want)
			}
		}
	})

	t.Run("nothing else changes", func(t *testing.T) {
		t.Parallel()
		original := decodeDoc(t, fixtureDoc(t, "StatefulSet"))
		patched := decodeDoc(t, chart.Objects[index].Doc)
		for _, body := range []map[string]any{original, patched} {
			stripResources(t, body)
		}
		if !reflect.DeepEqual(patched, original) {
			t.Errorf("the sizing changed more than resources:\n patched: %v\noriginal: %v", patched, original)
		}
		for _, object := range chart.Objects {
			if object.Kind == "StatefulSet" {
				continue
			}
			if want := fixtureDoc(t, object.Kind); !bytes.Equal(object.Doc, want) {
				t.Errorf("%s %s was rewritten", object.Kind, object.Name)
			}
		}
	})
}

// Every container and init container is sized, and a pod with no init
// containers is not an error.
func TestOpenShellGuaranteedSizesInitContainers(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name           string
		podSpec        string
		containers     int
		initContainers int
	}{
		{
			"containers and an init container",
			"      containers:\n        - name: a\n        - name: b\n      initContainers:\n        - name: setup\n",
			2, 1,
		},
		{"no init containers", "      containers:\n        - name: a\n", 1, 0},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var calls []runnerCall
			raw := []byte(chartWith(gatewayStatefulSetDoc(row.podSpec), openshellServiceDoc))
			chart, err := OpenShell(context.Background(), outputRunner(&calls, raw, nil), "chart", "values.yaml")
			if err != nil {
				t.Fatalf("OpenShell(): %v", err)
			}
			var gateway statefulSetDoc
			if err := yaml.Unmarshal(chart.Objects[0].Doc, &gateway); err != nil {
				t.Fatalf("parsing the patched StatefulSet: %v", err)
			}
			podSpec := gateway.Spec.Template.Spec
			if len(podSpec.Containers) != row.containers || len(podSpec.InitContainers) != row.initContainers {
				t.Fatalf("containers/initContainers = %d/%d, want %d/%d",
					len(podSpec.Containers), len(podSpec.InitContainers), row.containers, row.initContainers)
			}
			want := map[string]string{"cpu": gatewayCPU, "memory": gatewayMemory}
			for _, container := range slices.Concat(podSpec.Containers, podSpec.InitContainers) {
				if !maps.Equal(container.Resources.Requests, want) || !maps.Equal(container.Resources.Limits, want) {
					t.Errorf("container %s resources = %v/%v, want %v",
						container.Name, container.Resources.Requests, container.Resources.Limits, want)
				}
			}
		})
	}
}

// A chart output the sizing cannot place or size is refused with nothing
// returned.
func TestOpenShellGuaranteedRefusals(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		raw  string
		want string
	}{
		{"no StatefulSet", chartWith(openshellServiceDoc), "holds no StatefulSet named openshell"},
		{"two StatefulSets", chartWith(gatewayStatefulSetDoc(gatewayPod), gatewayStatefulSetDoc(gatewayPod)), "want one"},
		{
			"a StatefulSet with no spec.template",
			chartWith("apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: openshell\n  namespace: openshell\nspec: {}\n"),
			"no spec.template",
		},
		{
			"a StatefulSet whose spec is not a mapping",
			chartWith("apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: openshell\n  namespace: openshell\nspec: not-a-mapping\n"),
			"spec is not a mapping",
		},
		{
			"a StatefulSet whose template is not a mapping",
			chartWith("apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: openshell\n  namespace: openshell\nspec:\n  template: x\n"),
			"spec.template is not a mapping",
		},
		{"a pod with no containers", chartWith(gatewayStatefulSetDoc("      serviceAccountName: openshell\n")), "containers is missing"},
		{"a pod with an empty container list", chartWith(gatewayStatefulSetDoc("      containers: []\n")), "containers is empty"},
		{"containers that are not a sequence", chartWith(gatewayStatefulSetDoc("      containers: {}\n")), "containers is not a sequence"},
		{"a container that is not a mapping", chartWith(gatewayStatefulSetDoc("      containers:\n        - just-a-string\n")), "not a mapping"},
		{
			"init containers that are not a sequence",
			chartWith(gatewayStatefulSetDoc("      containers:\n        - name: a\n      initContainers: {}\n")),
			"initContainers is not a sequence",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var calls []runnerCall
			chart, err := OpenShell(context.Background(), outputRunner(&calls, []byte(row.raw), nil), "chart", "values.yaml")
			if err == nil {
				t.Fatalf("OpenShell() = %+v, nil error; want a refusal naming %s", chart, row.want)
			}
			if chart.Objects != nil || chart.Secrets != nil || chart.Hooks != nil {
				t.Errorf("OpenShell() returned %+v beside the error", chart)
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("error %q does not name %s", err, row.want)
			}
		})
	}
}

// A StatefulSet document the sizing cannot decode is refused with the document
// left as it was.
func TestOpenShellGuaranteedRefusesAnUndecodableDocument(t *testing.T) {
	t.Parallel()
	doc := []byte("apiVersion: [\n")
	chart := Chart{Objects: []k8s.StaticObject{{
		Kind: "StatefulSet", Name: gatewayStatefulSet, Doc: doc,
	}}}
	err := chart.applyGuaranteed(yaml.Marshal)
	if err == nil || !strings.Contains(err.Error(), gatewayStatefulSet) {
		t.Fatalf("applyGuaranteed() = %v, want a refusal naming %s", err, gatewayStatefulSet)
	}
	if !bytes.Equal(chart.Objects[0].Doc, doc) {
		t.Errorf("the document was rewritten beside the error")
	}
}

// A StatefulSet document the encoder cannot re-encode is refused with the
// document left as it was.
func TestOpenShellGuaranteedRefusesAnUnencodableDocument(t *testing.T) {
	t.Parallel()
	chart, err := parseChart([]byte(chartWith(gatewayStatefulSetDoc(gatewayPod), openshellServiceDoc)))
	if err != nil {
		t.Fatalf("parseChart(): %v", err)
	}
	doc := chart.Objects[0].Doc
	marshal := func(any) ([]byte, error) { return nil, errors.New("planted failure") }
	err = chart.applyGuaranteed(marshal)
	if err == nil || !strings.Contains(err.Error(), "planted failure") {
		t.Fatalf("applyGuaranteed() = %v, want the encoder's error", err)
	}
	if !strings.Contains(err.Error(), "re-encoding") {
		t.Errorf("error %q does not say which step failed", err)
	}
	if !bytes.Equal(chart.Objects[0].Doc, doc) {
		t.Errorf("the document was rewritten beside the error")
	}
}

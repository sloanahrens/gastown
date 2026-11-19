package sbx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

const openshellFixturePath = "testdata/openshell-trimmed.yaml"

func readChartFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(openshellFixturePath)
	if err != nil {
		t.Fatalf("reading %s: %v", openshellFixturePath, err)
	}
	return raw
}

// outputRunner returns raw and records every call, so a test can hold the
// render to the one command it may run.
func outputRunner(calls *[]runnerCall, raw []byte, err error) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, runnerCall{name: name, args: args})
		return raw, err
	}
}

type runnerCall struct {
	name string
	args []string
}

// chartOf joins documents the way helm does, so each row changes only the
// document it is about.
func chartOf(docs ...string) []byte {
	return []byte(strings.Join(docs, "\n---\n"))
}

const openshellServiceDoc = `apiVersion: v1
kind: Service
metadata:
  name: openshell
  namespace: openshell
spec:
  ports:
    - port: 50051
`

// certgenJobDoc is the hook Job with podSpec as its spec.template.spec.
func certgenJobDoc(podSpec string) string {
	return `apiVersion: batch/v1
kind: Job
metadata:
  name: openshell-certgen
  namespace: openshell
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
spec:
  template:
    spec:
` + podSpec
}

// The render runs exactly one command, `helm template` with the argv the design
// pins, and no other verb.
func TestOpenShellChartRunsHelmTemplateOnce(t *testing.T) {
	t.Parallel()
	var calls []runnerCall
	const chartDir = "deploy/cache/openshell"
	const valuesPath = "deploy/k8s/values/kind.yaml"
	if _, err := OpenShell(context.Background(), outputRunner(&calls, readChartFixture(t), nil), chartDir, valuesPath); err != nil {
		t.Fatalf("OpenShell(): %v", err)
	}
	want := runnerCall{
		name: "helm",
		args: []string{
			"template", "openshell", chartDir,
			"--namespace", "openshell",
			"-f", valuesPath,
			"--set", "agentSandbox.preflight.enabled=false",
		},
	}
	if len(calls) != 1 {
		t.Fatalf("the runner was called %d times (%v), want once", len(calls), calls)
	}
	if got := calls[0]; got.name != want.name || !slices.Equal(got.args, want.args) {
		t.Errorf("the runner was called with %s %q, want %s %q", got.name, got.args, want.name, want.args)
	}
}

// A failing helm is returned wrapped with what it wrote to stderr, and nothing
// is read as a chart.
func TestOpenShellChartRefusesARunnerFailure(t *testing.T) {
	t.Parallel()
	run := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New(`Error: chart "openshell" version "0.1.4" not found: the stderr text`)
	}
	chart, err := OpenShell(context.Background(), run, "chart", "values.yaml")
	if err == nil || !strings.Contains(err.Error(), "the stderr text") {
		t.Fatalf("OpenShell() = %+v, %v; want the runner's stderr text", chart, err)
	}
	if chart.Objects != nil || chart.Secrets != nil || chart.Hooks != nil {
		t.Errorf("OpenShell() returned %+v beside the error", chart)
	}
}

// The hook documents and the Secrets are left out of Objects and named instead.
func TestOpenShellChartDropsHooksAndSecrets(t *testing.T) {
	t.Parallel()
	var calls []runnerCall
	chart, err := OpenShell(context.Background(), outputRunner(&calls, readChartFixture(t), nil), "chart", "values.yaml")
	if err != nil {
		t.Fatalf("OpenShell(fixture): %v", err)
	}
	wantObjects := []struct {
		kind, name, namespace string
		cluster               bool
	}{
		{"StatefulSet", "openshell", "openshell", false},
		{"Service", "openshell", "openshell", false},
	}
	if len(chart.Objects) != len(wantObjects) {
		t.Fatalf("Objects = %d, want the fixture's %d kept documents", len(chart.Objects), len(wantObjects))
	}
	for i, want := range wantObjects {
		got := chart.Objects[i]
		if got.Kind != want.kind || got.Name != want.name || got.Namespace != want.namespace || got.Cluster != want.cluster {
			t.Errorf("object %d = %s %s (namespace %q, cluster %v), want %s %s (namespace %q, cluster %v)",
				i, got.Kind, got.Name, got.Namespace, got.Cluster, want.kind, want.name, want.namespace, want.cluster)
		}
	}

	wantHooks := []string{
		"ServiceAccount/openshell-certgen",
		"Role/openshell-certgen",
		"RoleBinding/openshell-certgen",
		"Job/openshell-certgen",
	}
	if !slices.Equal(chart.Hooks, wantHooks) {
		t.Errorf("Hooks = %q, want %q", chart.Hooks, wantHooks)
	}

	wantSecrets := []string{
		"openshell-credential-storage-key-encryption-key",
		"openshell-server-tls",
		"openshell-client-tls",
		"openshell-jwt-keys",
	}
	if !slices.Equal(chart.Secrets, wantSecrets) {
		t.Errorf("Secrets = %q, want %q", chart.Secrets, wantSecrets)
	}

	for _, object := range chart.Objects {
		if hasHookNode(object.Doc) {
			t.Errorf("%s %s in Objects carries the hook annotation", object.Kind, object.Name)
		}
	}
}

// hasHookNode re-reads a kept document for the hook annotation, so the
// assertion does not trust the drop it checks.
func hasHookNode(doc []byte) bool {
	node, err := documentNode(doc)
	if err != nil {
		return false
	}
	return hasHookAnnotation(node)
}

// No rejected document ever reaches the rendered output.
func TestOpenShellChartHoldsNoSecret(t *testing.T) {
	t.Parallel()
	raw := readChartFixture(t)
	if !bytes.Contains(raw, []byte("kind: Secret")) {
		t.Fatal("the fixture holds no Secret, so the drop is not exercised")
	}
	var calls []runnerCall
	chart, err := OpenShell(context.Background(), outputRunner(&calls, raw, nil), "chart", "values.yaml")
	if err != nil {
		t.Fatalf("OpenShell(fixture): %v", err)
	}
	for _, object := range chart.Objects {
		if object.Kind == "Secret" || bytes.Contains(object.Doc, []byte("kind: Secret")) {
			t.Errorf("Objects holds the Secret %s", object.Name)
		}
	}
}

// Every input the render cannot read, and every document it cannot place, is
// refused with no chart returned.
func TestOpenShellChartRefusals(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name                 string
		raw                  string
		noRunner             bool
		chartDir, valuesPath string
		want                 string
	}{
		{"no runner", "", true, "chart", "values.yaml", "no runner"},
		{"an empty chart directory", openshellServiceDoc, false, "", "values.yaml", "no chart directory"},
		{"a chart directory helm would read as a flag", openshellServiceDoc, false, "--set", "values.yaml", "read it as a flag"},
		{"an empty values file", openshellServiceDoc, false, "chart", "", "no values file"},
		{"a values file helm would read as a flag", openshellServiceDoc, false, "chart", "-f", "read it as a flag"},
		{"helm printing no document", "", false, "chart", "values.yaml", "printed no document"},
		{"a document that is not YAML", "apiVersion: [\n", false, "chart", "values.yaml", "not a YAML object"},
		{"a document that is not a mapping", "- one\n- two\n", false, "chart", "values.yaml", "not a YAML mapping"},
		{"no apiVersion", "kind: Service\nmetadata:\n  name: x\n  namespace: openshell\n", false, "chart", "values.yaml", "no apiVersion"},
		{"no kind", "apiVersion: v1\nmetadata:\n  name: x\n  namespace: openshell\n", false, "chart", "values.yaml", "no kind"},
		{"no metadata.name", "apiVersion: v1\nkind: Service\nmetadata:\n  namespace: openshell\n", false, "chart", "values.yaml", "no metadata.name"},
		{"a kind the chart does not carry", "apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: x\n  namespace: openshell\n", false, "chart", "values.yaml", "kind Ingress"},
		{"a namespaced kind with no namespace", "apiVersion: v1\nkind: Service\nmetadata:\n  name: x\n", false, "chart", "values.yaml", "metadata.namespace"},
		{"every document dropped", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: one\n  namespace: openshell\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: two\n  namespace: openshell\n", false, "chart", "values.yaml", "no object to apply"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var run Runner
			if !row.noRunner {
				var calls []runnerCall
				run = outputRunner(&calls, []byte(row.raw), nil)
			}
			chart, err := OpenShell(context.Background(), run, row.chartDir, row.valuesPath)
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

// The certgen Job names its Secrets in either helm spelling, in the order the
// flags are read.
func TestOpenShellChartReadsCertgenSecretFlags(t *testing.T) {
	t.Parallel()
	wantSecrets := []string{"openshell-server-tls", "openshell-client-tls", "openshell-jwt-keys"}
	rows := []struct{ name, podSpec string }{
		{"equals spelling", `      containers:
        - name: certgen
          args:
            - --server-secret-name=openshell-server-tls
            - --client-secret-name=openshell-client-tls
            - --jwt-secret-name=openshell-jwt-keys
`},
		{"separate values", `      containers:
        - name: certgen
          args:
            - --server-secret-name
            - openshell-server-tls
            - --client-secret-name
            - openshell-client-tls
            - --jwt-secret-name
            - openshell-jwt-keys
`},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var calls []runnerCall
			raw := chartOf(certgenJobDoc(row.podSpec), gatewayStatefulSetDoc(gatewayPod), openshellServiceDoc)
			chart, err := OpenShell(context.Background(), outputRunner(&calls, raw, nil), "chart", "values.yaml")
			if err != nil {
				t.Fatalf("OpenShell(): %v", err)
			}
			if !slices.Equal(chart.Secrets, wantSecrets) {
				t.Errorf("Secrets = %q, want %q", chart.Secrets, wantSecrets)
			}
		})
	}
}

// A certgen Job whose arguments cannot name its three Secrets is refused: the
// existence check would otherwise miss Secrets no document shows.
func TestOpenShellChartRefusesCertgenFlags(t *testing.T) {
	t.Parallel()
	rows := []struct{ name, chart, want string }{
		{
			"no hook Job",
			openshellServiceDoc,
			"gives no --server-secret-name",
		},
		{
			"a Job with no pod template",
			"apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: openshell-certgen\n  namespace: openshell\n  annotations:\n    helm.sh/hook: pre-install,pre-upgrade\nspec: {}\n",
			"no spec.template",
		},
		{
			"a Job with no containers",
			certgenJobDoc("      restartPolicy: Never\n"),
			"containers is missing or empty",
		},
		{
			"args that are not a sequence",
			certgenJobDoc("      containers:\n        - name: certgen\n          args: {}\n"),
			"args is not a sequence",
		},
		{
			"an arg that is not a scalar",
			certgenJobDoc("      containers:\n        - name: certgen\n          args:\n            - {}\n"),
			"arg is not a scalar",
		},
		{
			"a missing flag",
			certgenJobDoc(`      containers:
        - name: certgen
          args:
            - --server-secret-name=openshell-server-tls
            - --client-secret-name=openshell-client-tls
`),
			"gives no --jwt-secret-name",
		},
		{
			"an empty value",
			certgenJobDoc("      containers:\n        - name: certgen\n          args:\n            - --server-secret-name=\n            - --client-secret-name=c\n            - --jwt-secret-name=j\n"),
			"names an empty Secret",
		},
		{
			"a flag with no value",
			certgenJobDoc("      containers:\n        - name: certgen\n          args:\n            - --jwt-secret-name\n"),
			"names no value",
		},
		{
			"one flag naming two Secrets",
			certgenJobDoc(`      containers:
        - name: certgen
          args:
            - --server-secret-name=one
        - name: sidecar
          args:
            - --server-secret-name=two
            - --client-secret-name=c
            - --jwt-secret-name=j
`),
			"names both one and two",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var calls []runnerCall
			raw := chartOf(row.chart, openshellServiceDoc)
			chart, err := OpenShell(context.Background(), outputRunner(&calls, raw, nil), "chart", "values.yaml")
			if err == nil {
				t.Fatalf("OpenShell() = %+v, nil error; want a refusal naming %s", chart, row.want)
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("error %q does not name %s", err, row.want)
			}
		})
	}
}

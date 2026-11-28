package forgejorunner

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// controlClusterValues is values.control.yaml as the control cluster is meant to
// run: the values read on 2026-11-18, the engine's requests as the live cluster
// was patched to run them below its limit, and the operator's capacity of three
// (gt-jp8axa).
func controlClusterValues() Values {
	return Values{
		Namespace: "forgejo",
		Labels: []string{
			"docker-cli:docker://data.forgejo.org/oci/docker:28.5-cli",
			"node:docker://data.forgejo.org/oci/node:20.19",
			"ci-base:docker://ci-base:latest",
		},
		Capacity:        2,
		ForgejoHostIP:   "10.43.130.32",
		DindStorage:     "40Gi",
		RunnerStorage:   "1Gi",
		RunnerResources: Share{CPU: "1", Memory: "1Gi"},
		DindResources:   Share{CPU: "8", Memory: "12Gi"},
		DindRequests:    Share{CPU: "4", Memory: "8Gi"},
	}
}

// TestControlValuesBuild drives the production path: the embedded file decodes,
// passes validate, and the Values it yields build a manifest set.
func TestControlValuesBuild(t *testing.T) {
	t.Parallel()
	v, err := ControlValues()
	if err != nil {
		t.Fatalf("ControlValues: %v", err)
	}
	if _, err := Build(v); err != nil {
		t.Fatalf("Build(ControlValues()): %v", err)
	}
}

// TestControlValuesRenderTheEngineBelowItsLimit drives the control values through
// Build: the engine asks for the requests the file names and keeps the larger
// limit, so a tier sweep beside it still fits on the node.
func TestControlValuesRenderTheEngineBelowItsLimit(t *testing.T) {
	t.Parallel()
	v, err := ControlValues()
	if err != nil {
		t.Fatalf("ControlValues: %v", err)
	}
	m, err := Build(v)
	if err != nil {
		t.Fatalf("Build(ControlValues()): %v", err)
	}
	dind := containerNamed(t, m.Deployment.Spec.Template.Spec, DindContainerName)
	if want := (ResourceList{CPU: "4", Memory: "8Gi"}); dind.Resources.Requests != want {
		t.Errorf("the engine requests = %+v, want the control values' %+v", dind.Resources.Requests, want)
	}
	if want := (ResourceList{CPU: "8", Memory: "12Gi"}); dind.Resources.Limits != want {
		t.Errorf("the engine limits = %+v, want %+v", dind.Resources.Limits, want)
	}
	if got := decodeConfig(t, m).Runner.Envs["CI_CPU_QUOTA"]; got != "8" {
		t.Errorf("the job environment's CI_CPU_QUOTA = %q, want the engine limit 8", got)
	}
}

// TestControlValuesMatchTheCluster pins the file to the values the town means the
// control cluster to run, so an edit that changes one is a deliberate change to
// what the town asks the cluster for.
func TestControlValuesMatchTheCluster(t *testing.T) {
	t.Parallel()
	got, err := ControlValues()
	if err != nil {
		t.Fatalf("ControlValues: %v", err)
	}
	if want := controlClusterValues(); !reflect.DeepEqual(got, want) {
		t.Errorf("ControlValues() = %+v, want the cluster's values %+v", got, want)
	}
}

// TestControlValuesCapacity holds the control values' capacity to the two jobs
// the operator wants until the load scenario has run (gt-n1kfdu), both as the file
// sets it and as the config.yml the runner mounts carries it.
func TestControlValuesCapacity(t *testing.T) {
	t.Parallel()
	v, err := ControlValues()
	if err != nil {
		t.Fatalf("ControlValues: %v", err)
	}
	if v.Capacity != 2 {
		t.Errorf("ControlValues().Capacity = %d, want 2", v.Capacity)
	}
	if got := decodeConfig(t, mustBuild(t, v)).Runner.Capacity; got != 2 {
		t.Errorf("the rendered config.yml capacity = %d, want 2", got)
	}
}

// TestControlValuesRefusesABadFile drives every refusal: an unknown key at
// either level, a file with no document, one with two, and one whose values
// fail validate.
func TestControlValuesRefusesABadFile(t *testing.T) {
	t.Parallel()
	const valid = `namespace: forgejo
labels:
  - "gate:docker://gate:1"
forgejoHostIP: "10.0.0.7"
dindStorage: 40Gi
runnerStorage: 1Gi
runnerResources:
  cpu: "1"
  memory: 1Gi
dindResources:
  cpu: "8"
  memory: 12Gi
`
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"an unknown key", "dindCpu: 6\n", "dindCpu"},
		{"an unknown resource key", "runnerResources:\n  gpu: \"1\"\n", "gpu"},
		{"an empty file", "", "values.control.yaml"},
		{"malformed yaml", "labels: [\n", "values.control.yaml"},
		{"no labels", "forgejoHostIP: \"10.0.0.7\"\n", "Values.Labels"},
		{"a second document", valid + "---\ncapacity: 3\n", "one YAML document"},
		{"a second document that does not parse", valid + "---\nlabels: [\n", "values.control.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeValues("values.control.yaml", []byte(tc.body))
			if err == nil {
				t.Fatalf("decodeValues accepted %q", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestControlValuesFileNamesOnlyValuesFields reads the embedded bytes as a map
// and refuses a key Values does not have, at either level: a checked-in knob
// must be a knob the renderer can actually set.
func TestControlValuesFileNamesOnlyValuesFields(t *testing.T) {
	t.Parallel()
	var doc map[string]any
	if err := yaml.Unmarshal(controlValuesYAML, &doc); err != nil {
		t.Fatalf("values.control.yaml is not valid YAML: %v", err)
	}
	if len(doc) == 0 {
		t.Fatal("values.control.yaml holds no key, so the field check read nothing")
	}
	valuesType := reflect.TypeOf(Values{})
	for key, value := range doc {
		field, ok := fieldNamed(valuesType, key)
		if !ok {
			t.Errorf("values.control.yaml names %q, which is not a Values field", key)
			continue
		}
		nested, isMapping := value.(map[string]any)
		if !isMapping || field.Type.Kind() != reflect.Struct {
			continue
		}
		for nestedKey := range nested {
			if _, ok := fieldNamed(field.Type, nestedKey); !ok {
				t.Errorf("values.control.yaml names %s.%s, which is not a %s field", key, nestedKey, field.Type.Name())
			}
		}
	}
}

// TestControlValuesFileCarriesNoCredential keeps the checked-in file to what a
// drift compare needs: the scan that runs over rendered text runs here too.
func TestControlValuesFileCarriesNoCredential(t *testing.T) {
	t.Parallel()
	for _, finding := range scanSecretText(string(controlValuesYAML)) {
		t.Errorf("values.control.yaml carries %s; a checked-in values file holds no credential", finding)
	}
}

// TestDindRequestsDecode drives the strict decoder over the optional engine
// request share: a file naming dindRequests decodes it into Values.DindRequests,
// an unknown key under it is refused rather than dropped, and a file without it
// leaves the share empty so the validator takes the limit.
func TestDindRequestsDecode(t *testing.T) {
	t.Parallel()
	const base = `namespace: forgejo
labels:
  - "gate:docker://gate:1"
forgejoHostIP: "10.0.0.7"
dindStorage: 40Gi
runnerStorage: 1Gi
runnerResources:
  cpu: "1"
  memory: 1Gi
dindResources:
  cpu: "8"
  memory: 12Gi
`
	t.Run("decodes into Values.DindRequests", func(t *testing.T) {
		t.Parallel()
		v, err := decodeValues("values.control.yaml", []byte(base+"dindRequests:\n  cpu: \"4\"\n  memory: 8Gi\n"))
		if err != nil {
			t.Fatalf("decodeValues: %v", err)
		}
		if want := (Share{CPU: "4", Memory: "8Gi"}); v.DindRequests != want {
			t.Errorf("Values.DindRequests = %+v, want %+v", v.DindRequests, want)
		}
	})
	t.Run("an unknown key under it is refused", func(t *testing.T) {
		t.Parallel()
		_, err := decodeValues("values.control.yaml", []byte(base+"dindRequests:\n  gpu: \"1\"\n"))
		if err == nil {
			t.Fatal("decodeValues accepted an unknown key under dindRequests")
		}
		if !strings.Contains(err.Error(), "gpu") {
			t.Errorf("error = %q, want it to name %q", err, "gpu")
		}
	})
	t.Run("a file without it leaves the share empty", func(t *testing.T) {
		t.Parallel()
		v, err := decodeValues("values.control.yaml", []byte(base))
		if err != nil {
			t.Fatalf("decodeValues: %v", err)
		}
		if v.DindRequests != (Share{}) {
			t.Errorf("Values.DindRequests = %+v, want the zero share", v.DindRequests)
		}
	})
}

// TestAuxValuesRender drives values.aux.yaml through AuxValues and Build: every
// object carries the aux stem, the runner takes the one ci-aux label at capacity 1
// with the engine's quota, each container asks for the shares the file names, and
// the pod runs at the gate's priority class, which the sweep's preflight requires.
func TestAuxValuesRender(t *testing.T) {
	t.Parallel()
	v, err := AuxValues()
	if err != nil {
		t.Fatalf("AuxValues: %v", err)
	}
	m := mustBuild(t, v)
	const stem = "forgejo-runner-aux"
	for _, tc := range []struct{ name, got, want string }{
		{"the Deployment", m.Deployment.Metadata.Name, stem},
		{"the ConfigMap", m.ConfigMap.Metadata.Name, stem + "-config"},
		{"the engine claim", m.DindClaim.Metadata.Name, stem + "-dind"},
		{"the data claim", m.RunnerClaim.Metadata.Name, stem + "-data"},
		{"the namespace", m.Deployment.Metadata.Namespace, "forgejo"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	cfg := decodeConfig(t, m)
	if want := []string{"ci-aux:docker://ci-base:latest"}; !reflect.DeepEqual(cfg.Runner.Labels, want) {
		t.Errorf("labels = %q, want %q", cfg.Runner.Labels, want)
	}
	if cfg.Runner.Capacity != 1 {
		t.Errorf("capacity = %d, want 1", cfg.Runner.Capacity)
	}
	if got := cfg.Runner.Envs["CI_CPU_QUOTA"]; got != "4" {
		t.Errorf("CI_CPU_QUOTA = %q, want the engine limit 4", got)
	}
	pod := m.Deployment.Spec.Template.Spec
	if pod.PriorityClassName != PriorityClassName {
		t.Errorf("priorityClassName = %q, want the gate runner's %q", pod.PriorityClassName, PriorityClassName)
	}
	for _, tc := range []struct {
		container        string
		requests, limits ResourceList
	}{
		{runnerContainerName, ResourceList{CPU: "200m", Memory: "256Mi"}, ResourceList{CPU: "200m", Memory: "256Mi"}},
		{DindContainerName, ResourceList{CPU: "500m", Memory: "2Gi"}, ResourceList{CPU: "4", Memory: "6Gi"}},
	} {
		c := containerNamed(t, pod, tc.container)
		if c.Resources.Requests != tc.requests {
			t.Errorf("%s requests = %+v, want %+v", tc.container, c.Resources.Requests, tc.requests)
		}
		if c.Resources.Limits != tc.limits {
			t.Errorf("%s limits = %+v, want %+v", tc.container, c.Resources.Limits, tc.limits)
		}
	}
}

// TestAuxValuesRefusesABadFile drives the two refusals the aux file can take: a key
// Values does not have, and a second document. Each row appends its defect to the
// embedded bytes, so the test reads the file the build reads.
func TestAuxValuesRefusesABadFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, extra, want string }{
		{"an unknown key", "dindCpu: 6\n", "dindCpu"},
		{"a second document", "---\ncapacity: 3\n", "one YAML document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeValues("values.aux.yaml", []byte(string(auxValuesYAML)+tc.extra))
			if err == nil {
				t.Fatalf("decodeValues accepted the aux file with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// fieldNamed returns the field of t whose name matches key case-insensitively,
// which is how a yaml key maps onto the value it sets.
func fieldNamed(t reflect.Type, key string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if strings.EqualFold(field.Name, key) {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

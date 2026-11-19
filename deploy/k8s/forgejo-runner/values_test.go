package forgejorunner

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// controlClusterValues is values.control.yaml as the Goal records the control
// cluster on 2026-11-18.
func controlClusterValues() Values {
	return Values{
		Namespace: "forgejo",
		Labels: []string{
			"docker-cli:docker://data.forgejo.org/oci/docker:28.5-cli",
			"node:docker://data.forgejo.org/oci/node:20.19",
			"ci-base:docker://data.forgejo.org/oci/gastown-runner:go1.27-node20",
		},
		Capacity:        2,
		ForgejoHostIP:   "10.43.130.32",
		DindStorage:     "40Gi",
		RunnerStorage:   "1Gi",
		RunnerResources: Share{CPU: "1", Memory: "1Gi"},
		DindResources:   Share{CPU: "8", Memory: "12Gi"},
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

// TestControlValuesMatchTheCluster pins the file to the values the Goal records
// from the control cluster, so an edit that changes one is a deliberate change
// to what the town believes the cluster runs.
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
			_, err := decodeControlValues([]byte(tc.body))
			if err == nil {
				t.Fatalf("decodeControlValues accepted %q", tc.body)
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

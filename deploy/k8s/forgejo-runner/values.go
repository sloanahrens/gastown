package forgejorunner

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// controlValuesYAML is values.control.yaml, compiled in so the tests and the
// cluster's drift compare decode the exact bytes the repository commits.
//
//go:embed values.control.yaml
var controlValuesYAML []byte

// auxValuesYAML is values.aux.yaml, the second runner's file, compiled in the same
// way so its tests decode the committed bytes (gt-juwsv6).
//
//go:embed values.aux.yaml
var auxValuesYAML []byte

// fileValues is the shape of a values file: Values' fields named the way an
// operator writes them, since Values itself carries no yaml tags. Strict decoding
// refuses a key this struct does not name, so a knob cannot exist in a file before
// it exists in Values. A file that omits name takes the constant stem.
type fileValues struct {
	Name            string   `yaml:"name"`
	Namespace       string   `yaml:"namespace"`
	Labels          []string `yaml:"labels"`
	Capacity        int      `yaml:"capacity"`
	ForgejoHostIP   string   `yaml:"forgejoHostIP"`
	DindStorage     string   `yaml:"dindStorage"`
	RunnerStorage   string   `yaml:"runnerStorage"`
	RunnerResources Share    `yaml:"runnerResources"`
	DindResources   Share    `yaml:"dindResources"`
	DindRequests    Share    `yaml:"dindRequests"`
	CPUQuota        string   `yaml:"cpuQuota"`
}

// ControlValues returns the Values the control cluster's runner is built from:
// values.control.yaml decoded strictly and validated as written, so a misspelled
// key or a missing required setting is an error rather than a default that
// differs from the cluster.
func ControlValues() (Values, error) {
	return decodeValues("values.control.yaml", controlValuesYAML)
}

// AuxValues returns the Values the aux runner is built from: values.aux.yaml,
// decoded and validated by the same decoder as ControlValues (gt-juwsv6).
func AuxValues() (Values, error) {
	return decodeValues("values.aux.yaml", auxValuesYAML)
}

// decodeValues reads one YAML document into Values; file names the document in
// every error. Every key must name a field, and a second document is refused, since
// decoding it would silently drop what it holds.
func decodeValues(file string, data []byte) (Values, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc fileValues
	if err := dec.Decode(&doc); err != nil {
		return Values{}, fmt.Errorf("reading %s: %w", file, err)
	}
	var extra any
	switch err := dec.Decode(&extra); {
	case err == nil:
		return Values{}, fmt.Errorf("reading %s: want one YAML document; the file holds a second", file)
	case !errors.Is(err, io.EOF):
		return Values{}, fmt.Errorf("reading %s: %w", file, err)
	}
	v := Values{
		Name:            doc.Name,
		Namespace:       doc.Namespace,
		Labels:          doc.Labels,
		Capacity:        doc.Capacity,
		ForgejoHostIP:   doc.ForgejoHostIP,
		DindStorage:     doc.DindStorage,
		RunnerStorage:   doc.RunnerStorage,
		RunnerResources: doc.RunnerResources,
		DindResources:   doc.DindResources,
		DindRequests:    doc.DindRequests,
		CPUQuota:        doc.CPUQuota,
	}
	if err := v.validate(); err != nil {
		return Values{}, fmt.Errorf("%s: %w", file, err)
	}
	return v, nil
}

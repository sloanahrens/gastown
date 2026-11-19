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

// controlValues is the shape of values.control.yaml: Values' fields named the
// way an operator writes them, since Values itself carries no yaml tags. Strict
// decoding refuses a key this struct does not name, and the test pins every key
// to a Values field so a knob cannot exist here before it exists there.
type controlValues struct {
	Namespace       string   `yaml:"namespace"`
	Labels          []string `yaml:"labels"`
	Capacity        int      `yaml:"capacity"`
	ForgejoHostIP   string   `yaml:"forgejoHostIP"`
	DindStorage     string   `yaml:"dindStorage"`
	RunnerStorage   string   `yaml:"runnerStorage"`
	RunnerResources Share    `yaml:"runnerResources"`
	DindResources   Share    `yaml:"dindResources"`
}

// ControlValues returns the Values the control cluster's runner is built from:
// values.control.yaml decoded strictly and validated as written, so a misspelled
// key or a missing required setting is an error rather than a default that
// differs from the cluster.
func ControlValues() (Values, error) {
	return decodeControlValues(controlValuesYAML)
}

// decodeControlValues reads one YAML document into Values. Every key must name
// a field; a second document is refused, since decoding it would silently drop
// what it holds.
func decodeControlValues(data []byte) (Values, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc controlValues
	if err := dec.Decode(&doc); err != nil {
		return Values{}, fmt.Errorf("reading values.control.yaml: %w", err)
	}
	var extra any
	switch err := dec.Decode(&extra); {
	case err == nil:
		return Values{}, errors.New("reading values.control.yaml: want one YAML document; the file holds a second")
	case !errors.Is(err, io.EOF):
		return Values{}, fmt.Errorf("reading values.control.yaml: %w", err)
	}
	v := Values{
		Namespace:       doc.Namespace,
		Labels:          doc.Labels,
		Capacity:        doc.Capacity,
		ForgejoHostIP:   doc.ForgejoHostIP,
		DindStorage:     doc.DindStorage,
		RunnerStorage:   doc.RunnerStorage,
		RunnerResources: doc.RunnerResources,
		DindResources:   doc.DindResources,
	}
	if err := v.validate(); err != nil {
		return Values{}, fmt.Errorf("values.control.yaml: %w", err)
	}
	return v, nil
}

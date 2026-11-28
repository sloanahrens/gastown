package forgejoserver

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// serverYAML is server.yaml, compiled in so the tests decode the exact bytes the
// repository commits.
//
//go:embed server.yaml
var serverYAML []byte

// Objects returns the documents of server.yaml as JSON-kind maps, in file order.
func Objects() ([]map[string]any, error) {
	return decodeObjects(serverYAML)
}

// decodeObjects splits a multi-document YAML stream into JSON-kind maps. A
// document that does not parse, is empty, or is not an object is an error.
func decodeObjects(raw []byte) ([]map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var objs []map[string]any
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return objs, nil
		}
		n := len(objs) + 1
		if err != nil {
			return nil, fmt.Errorf("server.yaml document %d: %w", n, err)
		}
		encoded, err := json.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("server.yaml document %d: %w", n, err)
		}
		var obj map[string]any
		if err := json.Unmarshal(encoded, &obj); err != nil {
			return nil, fmt.Errorf("server.yaml document %d is not an object: %w", n, err)
		}
		if obj == nil {
			return nil, fmt.Errorf("server.yaml document %d is empty", n)
		}
		objs = append(objs, obj)
	}
}

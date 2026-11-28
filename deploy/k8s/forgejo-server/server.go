package forgejoserver

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"gopkg.in/yaml.v3"
)

// serverYAML is server.yaml, compiled in so the tests decode the exact bytes the
// repository commits.
//
//go:embed server.yaml
var serverYAML []byte

// databaseHostPlaceholder is the unfilled FORGEJO__database__HOST that
// server.yaml holds until Manifest fills it from the environment's hosts file.
const databaseHostPlaceholder = "CONTROL_PLANE_HOST:5442"

// hostName is the shape a database host may take.
var hostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Manifest returns server.yaml with its one databaseHostPlaceholder replaced by
// controlPlaneHost and the port 5442.
func Manifest(controlPlaneHost string) ([]byte, error) {
	if err := checkHost(controlPlaneHost); err != nil {
		return nil, err
	}
	return fillHost(serverYAML, controlPlaneHost)
}

// Objects returns the documents of Manifest's result as JSON-kind maps, in file
// order.
func Objects(controlPlaneHost string) ([]map[string]any, error) {
	raw, err := Manifest(controlPlaneHost)
	if err != nil {
		return nil, err
	}
	return decodeObjects(raw)
}

// checkHost refuses a host no environment may name: empty, over the 253 a DNS
// name holds, outside hostName's shape, or the placeholder's own name.
func checkHost(host string) error {
	switch {
	case host == "":
		return errors.New("database host is empty")
	case len(host) > 253:
		return fmt.Errorf("database host is %d characters, over the 253 a DNS name may hold", len(host))
	case !hostName.MatchString(host):
		return fmt.Errorf("database host %q is not of the shape [A-Za-z0-9][A-Za-z0-9._-]*", host)
	case host == "CONTROL_PLANE_HOST":
		return errors.New("database host is the unfilled placeholder CONTROL_PLANE_HOST")
	}
	return nil
}

// fillHost returns raw with its one placeholder replaced by controlPlaneHost and
// the port; raw is a parameter so a test can pass bytes it wrote.
func fillHost(raw []byte, controlPlaneHost string) ([]byte, error) {
	if n := bytes.Count(raw, []byte(databaseHostPlaceholder)); n != 1 {
		return nil, fmt.Errorf("server.yaml holds the database host placeholder %d times, want exactly one", n)
	}
	return bytes.Replace(raw, []byte(databaseHostPlaceholder), []byte(controlPlaneHost+":5442"), 1), nil
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

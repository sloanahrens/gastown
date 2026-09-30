package beads

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ParseConfigGetJSON reads the value from bd config get --json: the machine
// envelope's data, or the legacy bare {key, value, location} object. Output
// with no value is an error, never "".
func ParseConfigGetJSON(out []byte) (string, error) {
	body := bytes.TrimSpace(out)
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &env) == nil && len(env.Data) > 0 && string(env.Data) != "null" {
		body = env.Data
	}
	var kv struct {
		Value *string `json:"value"`
	}
	if err := json.Unmarshal(body, &kv); err != nil || kv.Value == nil {
		first, _, _ := strings.Cut(string(out), "\n")
		return "", fmt.Errorf("bd config get --json: no value in %q", first)
	}
	return strings.TrimSpace(*kv.Value), nil
}

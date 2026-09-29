package beads

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// machineEnvelope is the one JSON document bd writes under machine mode
// (BD_MACHINE=1): the legacy --json payload in data, paging in pagination,
// a typed failure in error (beads engdocs/design/d1-machine-surface.md).
type machineEnvelope struct {
	SchemaVersion   int                `json:"schema_version"`
	ContractVersion int                `json:"contract_version"`
	Data            json.RawMessage    `json:"data"`
	Pagination      *machinePagination `json:"pagination"`
	Error           *machineError      `json:"error"`
}

type machinePagination struct {
	Returned   int    `json:"returned"`
	Total      *int   `json:"total,omitempty"`
	Truncated  bool   `json:"truncated"`
	Limit      int    `json:"limit,omitempty"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type machineError struct {
	Kind    string          `json:"kind"`
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// decodeMachineEnvelope reads bd's machine-mode output. ok is false when out
// is JSON but not an envelope: a bd from before machine mode ignores
// BD_MACHINE and prints its legacy --json payload, which the caller may
// still accept. Output that is not JSON at all is an error (RequireJSON),
// and so is an envelope carrying an error.
func decodeMachineEnvelope(out []byte, what string) (env machineEnvelope, ok bool, err error) {
	if err := RequireJSON(out, what); err != nil {
		return env, false, err
	}
	trimmed := bytes.TrimSpace(out)
	if trimmed[0] != '{' {
		return env, false, nil
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(trimmed, &probe) != nil {
		return env, false, nil
	}
	if _, hasData := probe["data"]; !hasData {
		return env, false, nil
	}
	if _, hasContract := probe["contract_version"]; !hasContract {
		return env, false, nil
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return env, false, fmt.Errorf("%s: parsing bd machine envelope: %w", what, err)
	}
	if env.Error != nil && env.Error.Kind != "" {
		return env, true, fmt.Errorf("%s: bd reported %s: %s", what, env.Error.Kind, env.Error.Message)
	}
	return env, true, nil
}

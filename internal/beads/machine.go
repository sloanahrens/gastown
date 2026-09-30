package beads

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// machineEnvEntry puts bd in machine mode: exact ids (no prefix resolution, so
// a short id never matches the wrong bead), the envelope above on stdout, typed
// exit statuses, and no TTY, UI, metrics, plugins or stdin prompts
// (docs/adr/0001-cli-only-beads-access.md).
const machineEnvEntry = "BD_MACHINE=1"

// WithMachineEnv returns env with BD_MACHINE=1 set exactly once. Every bd
// subprocess gastown starts carries it: the policy builders add it through
// SuppressBDSideEffects, and the Command* constructors that take the caller's
// env add it here.
func WithMachineEnv(env []string) []string {
	return WithMachineEnvIn("", env)
}

// WithMachineEnvIn is WithMachineEnv for a command that runs in dir. A nil env
// means the parent's environment, which exec.Cmd reads with PWD moved to dir;
// expanding it here keeps that.
func WithMachineEnvIn(dir string, env []string) []string {
	if env == nil {
		env = os.Environ()
		if dir != "" {
			if pwd, err := filepath.Abs(dir); err == nil {
				env = append(env, "PWD="+pwd)
			}
		}
	}
	return append(StripEnvKey(env, "BD_MACHINE"), machineEnvEntry)
}

// WithoutMachineEnv is the one way out of machine mode, for a bd call whose
// stdout is not parsed as JSON: one that hands the operator's terminal over
// (gt show execs bd show) prints for a person, and an envelope is not that.
// Each caller is named in machineExemptCallers (machine_policy_test.go).
func WithoutMachineEnv(env []string) []string {
	return StripEnvKey(env, "BD_MACHINE")
}

// machineExempt reports whether a call runs outside machine mode whatever its
// caller did. bd sql is the only verb: machine mode prints its rows as JSON
// objects with the keys sorted, which loses the SELECT order the csv readers
// in internal/doctor index by.
func machineExempt(args []string) bool {
	rest, ok := stripBDGlobalFlags(args)
	return ok && len(rest) > 0 && rest[0] == "sql"
}

// machineEnvForCall is WithMachineEnvIn for a call with a known argv: the
// exempt verbs get the environment without machine mode.
func machineEnvForCall(dir string, env, args []string) []string {
	env = WithMachineEnvIn(dir, env)
	if machineExempt(args) {
		return WithoutMachineEnv(env)
	}
	return env
}

// LegacyPayload turns what bd printed under machine mode into what the same
// call printed before it: the envelope's data, exactly as the --json payload
// was. Output that is not an envelope passes through, so a bd from before
// machine mode and the error path (whose envelope the typed-failure code in
// bd_failure.go reads) behave as they did. args is the argv after "bd".
//
// One command changes shape rather than wrapping: create --silent printed the
// bare id and, under machine mode, prints the created issue.
func LegacyPayload(args []string, out []byte) []byte {
	env, ok, err := decodeMachineEnvelope(out, "bd")
	if err != nil || !ok || len(env.Data) == 0 {
		return out
	}
	if slices.Contains(args, "--silent") {
		var created struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(env.Data, &created) == nil && created.ID != "" {
			return []byte(created.ID + "\n")
		}
	}
	return []byte(env.Data)
}

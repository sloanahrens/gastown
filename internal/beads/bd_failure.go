package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Exit statuses bd's machine mode reserves for typed failures (beads
// engdocs/design/d1-machine-surface.md; bd capabilities --json error_kinds).
const (
	bdNotFoundExit         = 20
	bdRouteUnreachableExit = 24
	bdStoreUnavailableExit = 25
	bdSchemaSkewExit       = 26
)

// ErrUnavailable marks a bd call that failed without bd saying the bead does
// not exist: the store, the route, the schema, the binary or the arguments
// failed. The call's answer is unknown; it is never evidence of absence.
var ErrUnavailable = errors.New("bd could not answer")

// bdSaidNotFound reports whether a failed bd call is bd saying the id does
// not exist, as opposed to bd being unable to answer. Only machine mode
// answers that: exit status not_found (20), or an envelope of kind
// "not_found" on stdout. Every other failure, whatever its text, is not an
// absence: "not found" is also what Dolt prints when a database or table is
// missing (G3-01, G5-02).
func bdSaidNotFound(exitCode int, stdout []byte) bool {
	if exitCode == bdNotFoundExit {
		return true
	}
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var env struct {
		Error *struct {
			Kind string `json:"kind"`
		} `json:"error"`
	}
	return json.Unmarshal(trimmed, &env) == nil && env.Error != nil && env.Error.Kind == "not_found"
}

// markUnavailable returns err carrying ErrUnavailable, so a caller cannot read
// a failed read as an empty one. An error that already carries ErrUnavailable
// comes back unchanged.
func markUnavailable(err error) error {
	if err == nil || errors.Is(err, ErrUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// exitCodeOf returns err's process exit status, or -1 when it has none.
func exitCodeOf(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}

// unavailableError is a bd failure that is not a confirmed absence. It keeps
// the message a caller already sees and exposes both the process error and
// ErrUnavailable.
type unavailableError struct {
	msg   string
	cause error
	// stdout is bd's stdout, which in machine mode is the error envelope.
	stdout []byte
}

func (e *unavailableError) Error() string { return e.msg }

func (e *unavailableError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrUnavailable}
	}
	return []error{e.cause, ErrUnavailable}
}

// bdErrorID is one failed id in a machine-mode envelope's error.ids.
type bdErrorID struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// machineErrorOf reads the machine-mode envelope a failed bd call left on
// stdout: its error kind and the per-id failures of a batch. ok is false when
// the call left no envelope (a bd without machine mode, or a failure before
// bd wrote one).
func machineErrorOf(err error) (kind string, ids []bdErrorID, ok bool) {
	var ue *unavailableError
	if !errors.As(err, &ue) {
		return "", nil, false
	}
	trimmed := bytes.TrimSpace(ue.stdout)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", nil, false
	}
	var env struct {
		ContractVersion int `json:"contract_version"`
		Error           *struct {
			Kind string      `json:"kind"`
			IDs  []bdErrorID `json:"ids"`
		} `json:"error"`
	}
	if json.Unmarshal(trimmed, &env) != nil || env.ContractVersion == 0 || env.Error == nil || env.Error.Kind == "" {
		return "", nil, false
	}
	return env.Error.Kind, env.Error.IDs, true
}

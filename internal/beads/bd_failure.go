package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
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

// bdNotFoundSentence matches the sentences bd itself writes for an unknown
// issue or wisp id. Each names the bead ("issue <id> not found", "issue not
// found: <id>", "no issue found matching ..."), so Dolt's "database not
// found", "table not found", "column not found", an exec "executable file
// not found" or a bare "not found" never match.
var bdNotFoundSentence = regexp.MustCompile(`(?i)\b(?:issue|wisp)\s+'?[a-z0-9][\w.\-]*'?\s+not found\b|\b(?:issue|wisp) not found\b|\bno issues? found matching\b`)

// BDReportedNotFound reports whether a failed bd call is bd saying the id
// does not exist, as opposed to bd being unable to answer. The authority, in
// order: bd's machine-mode exit status (not_found = 20; the other typed exits
// are never absence), a JSON error on stdout (envelope kind "not_found", or
// the legacy --json error sentence), then bd's own not-found sentence on
// stderr, which pre-machine-mode bd builds exit 1 with. Nothing else counts:
// a substring "not found" is what Dolt prints when a database or table is
// missing (G3-01, G5-02).
func BDReportedNotFound(exitCode int, stdout, stderr []byte) bool {
	switch exitCode {
	case bdNotFoundExit:
		return true
	case bdGuardNotHeldExit, bdRouteUnreachableExit, bdStoreUnavailableExit, bdSchemaSkewExit:
		return false
	}
	if kind, msg, ok := jsonErrorOf(stdout); ok {
		if kind != "" {
			return kind == "not_found"
		}
		return bdNotFoundSentence.MatchString(msg)
	}
	for _, line := range strings.Split(string(stderr), "\n") {
		if bdNotFoundSentence.MatchString(line) {
			return true
		}
	}
	return false
}

// jsonErrorOf extracts the error bd put on stdout for a --json call: the
// machine envelope's error.kind/message, or the legacy {"error": "..."}.
func jsonErrorOf(stdout []byte) (kind, msg string, ok bool) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", "", false
	}
	var obj struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(trimmed, &obj) != nil || len(obj.Error) == 0 || string(obj.Error) == "null" {
		return "", "", false
	}
	var typed struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}
	if json.Unmarshal(obj.Error, &typed) == nil && typed.Kind != "" {
		return typed.Kind, typed.Message, true
	}
	var legacy string
	if json.Unmarshal(obj.Error, &legacy) == nil {
		return "", legacy, true
	}
	return "", "", false
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
}

func (e *unavailableError) Error() string { return e.msg }

func (e *unavailableError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrUnavailable}
	}
	return []error{e.cause, ErrUnavailable}
}

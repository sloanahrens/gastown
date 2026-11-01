package birdreports

import (
	_ "embed"
	"strings"
)

// dailyRequest is the message the overseer sends a reporter whose daily report
// is due. docs/birds/daily-request.md is the source; go:embed cannot name a
// file outside this package's directory, so the copy beside this file holds the
// same bytes, and TestEmbeddedRequestIsTheDocsCopy fails if the two differ.
//
//go:embed daily-request.md
var dailyRequest string

// Request returns the request text addressed to reporter, telling it to publish
// under runtimeDir: every <NAME> is replaced with the reporter's name and every
// <RUNTIME_DIR> with the runtime directory the caller reads reports from, so
// the request names the path the reader looks at rather than a fixed one.
func Request(reporter, runtimeDir string) string {
	return strings.NewReplacer(
		"<NAME>", reporter,
		"<RUNTIME_DIR>", runtimeDir,
	).Replace(dailyRequest)
}

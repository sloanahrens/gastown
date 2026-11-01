package birdreports

import (
	_ "embed"
	"strings"
)

// dailyRequest is the message the overseer sends a reporter whose daily report
// is due, under the pointer header the copy carries. docs/birds/daily-request.md
// is the source; go:embed cannot name a file outside this package's directory,
// so the copy beside this file holds the same text, and
// TestEmbeddedRequestIsTheDocsCopy fails if the two differ.
//
//go:embed daily-request.md
var dailyRequest string

// pointerClose ends the pointer comment the copy opens with, the one naming
// docs/birds/daily-request.md. Request cuts through it, so the request a
// reporter is sent carries no note about how this package is built.
const pointerClose = "-->\n\n"

// Request returns the request text addressed to reporter, telling it to publish
// under runtimeDir: every <NAME> is replaced with the reporter's name and every
// <RUNTIME_DIR> with the runtime directory the caller reads reports from, so
// the request names the path the reader looks at rather than a fixed one.
func Request(reporter, runtimeDir string) string {
	text := dailyRequest
	if i := strings.Index(text, pointerClose); i >= 0 {
		text = text[i+len(pointerClose):]
	}
	return strings.NewReplacer(
		"<NAME>", reporter,
		"<RUNTIME_DIR>", runtimeDir,
	).Replace(text)
}

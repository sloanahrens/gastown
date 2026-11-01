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

// Request returns the request text addressed to reporter: every <NAME> in it
// is replaced with the reporter's name.
func Request(reporter string) string {
	return strings.ReplaceAll(dailyRequest, "<NAME>", reporter)
}

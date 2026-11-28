package presubmit

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// testEvent is the part of a `go test -json` event the red-on-base checks read.
type testEvent struct {
	Action string
	Test   string
}

// RedBaseEvents is what one package's `go test -json` output says about its top-level tests and its own result (gt-gxnlab).
type RedBaseEvents struct {
	Passed []string // top-level tests with a pass event, sorted and once each
	Ran    []string // top-level tests with a pass, fail or skip event, sorted and once each
	// FailedBuild is set when the package-level result, the event carrying no Test, is fail.
	FailedBuild bool
}

// RedBaseRan reads out, the bytes `go test -json` printed, for the top-level tests that ran and whether the package failed (gt-gxnlab). A subtest event (a Test holding a /) names no top-level test, and a test-level fail is not the package result. go test writes the package fail event for a build or setup failure too, so a package it never loaded still reads as failed. A line that is not JSON, as a build failure prints, is skipped; the output is split on newlines rather than read with a bufio.Scanner, so one long line cannot end the scan early.
func RedBaseRan(out []byte) RedBaseEvents {
	var res RedBaseEvents
	passed, ran := map[string]bool{}, map[string]bool{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		var ev testEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Action != "pass" && ev.Action != "fail" && ev.Action != "skip" {
			continue
		}
		if ev.Test == "" {
			if ev.Action == "fail" {
				res.FailedBuild = true
			}
			continue
		}
		if strings.Contains(ev.Test, "/") {
			continue
		}
		ran[ev.Test] = true
		if ev.Action == "pass" {
			passed[ev.Test] = true
		}
	}
	res.Passed = slices.Sorted(maps.Keys(passed))
	res.Ran = slices.Sorted(maps.Keys(ran))
	return res
}

// RedBasePassed names, sorted and once each, the top-level tests with a pass event in out, the bytes `go test -json` printed (gt-32afsw).
func RedBasePassed(out []byte) []string {
	return RedBaseRan(out).Passed
}

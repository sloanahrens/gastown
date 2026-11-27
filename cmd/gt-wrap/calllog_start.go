package main

import (
	"io"
	"path/filepath"
)

// startCallLog builds the call log at <town>/.runtime/gt-wrap/calls.log, reads
// the call's start time from the log's clock, and returns the function that
// records the call's status and the duration from the same clock. A nil newLog,
// or an empty town (the forwarder with no gateway), builds no log, so the
// returned function does nothing.
func startCallLog(newLog func(path string) *callLog, stderr io.Writer, town, tool string, args []string, dir string) func(status int) {
	if newLog == nil || town == "" {
		return func(int) {}
	}
	l := newLog(filepath.Join(town, ".runtime/gt-wrap/calls.log"))
	start := l.now()
	return func(status int) {
		l.record(stderr, tool, args, status, l.now().Sub(start), dir)
	}
}

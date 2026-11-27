package main

import (
	"bytes"
	"testing"
	"time"
)

// TestCallLogStartRecords: the log is built under the town, nothing is written
// until the returned function is called, and it then records the status and the
// duration the log's clock measured.
func TestCallLogStartRecords(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 26, 20, 27, 3, 0, time.UTC)
	f := newFakeCallLog(0)
	f.now = func() time.Time { return clock }
	var path string
	finish := startCallLog(func(p string) *callLog { path = p; return f.callLog }, &bytes.Buffer{},
		"/gt/gastown", "bd", []string{"create", "bead"}, "/gt/gastown")
	if f.file.written != "" {
		t.Fatalf("wrote %q before the returned function was called", f.file.written)
	}
	clock = clock.Add(1500 * time.Millisecond)
	finish(3)
	if want := "/gt/gastown/.runtime/gt-wrap/calls.log"; path != want {
		t.Errorf("log path = %q, want %q", path, want)
	}
	if want := "2026-11-26T20:27:04Z\tbd\tcreate bead\t3\t1500\t/gt/gastown\n"; f.file.written != want {
		t.Errorf("line = %q, want %q", f.file.written, want)
	}
}

// TestCallLogStartNoLog: a nil newLog and an empty town build no log and note nothing.
func TestCallLogStartNoLog(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		town   string
		newLog func(string) *callLog
	}{
		{"nil newLog", "/gt/gastown", nil},
		{"empty town", "", func(string) *callLog { t.Fatal("built a log with no town"); return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			startCallLog(tc.newLog, &stderr, tc.town, "bd", nil, "/d")(1)
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing", stderr.String())
			}
		})
	}
}

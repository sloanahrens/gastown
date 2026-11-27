package main

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

// TestCallLogRecordAppends: record makes the log's directory, appends its line
// with the status, duration and directory, and says nothing.
func TestCallLogRecordAppends(t *testing.T) {
	t.Parallel()
	f := newFakeCallLog(0)
	var stderr bytes.Buffer
	f.record(&stderr, "bd", []string{"create", "bead"}, 1, 40*time.Millisecond, "/gt/gastown")
	if f.madeDir != "/log" {
		t.Errorf("made directory %q, want /log", f.madeDir)
	}
	if want := "2026-11-26T20:27:03Z\tbd\tcreate bead\t1\t40\t/gt/gastown\n"; f.file.written != want {
		t.Errorf("line = %q, want %q", f.file.written, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

// TestCallLogRecordFailed: a directory that cannot be made, and a line that
// cannot be appended, are each one note on stderr and no line.
func TestCallLogRecordFailed(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		fail func(*fakeCallLog)
		want string
	}{
		{"mkdir", func(f *fakeCallLog) { f.mkdir = func(string, fs.FileMode) error { return boom } },
			"gt-wrap: the call was not logged: make call log directory /log: boom\n"},
		{"append", func(f *fakeCallLog) { f.open = func(string) (callLogAppender, error) { return nil, boom } },
			"gt-wrap: the call was not logged: gt-wrap: append call log /log/calls.log: boom\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeCallLog(0)
			tc.fail(f)
			var stderr bytes.Buffer
			f.record(&stderr, "bd", nil, 0, 0, "/d")
			if got := stderr.String(); got != tc.want {
				t.Errorf("stderr = %q, want %q", got, tc.want)
			}
			if f.file.written != "" {
				t.Errorf("appended %q; a failed record appends nothing", f.file.written)
			}
		})
	}
}

// TestCallLogRecordOSSeam: osCallLog's directory seam makes every parent level.
func TestCallLogRecordOSSeam(t *testing.T) {
	t.Parallel()
	l := osCallLog(filepath.Join(t.TempDir(), "town", "state", "calls.log"))
	if l.mkdir == nil {
		t.Fatal("osCallLog leaves the directory seam unset")
	}
	if err := l.mkdir(filepath.Dir(l.path), callLogDirMode); err != nil {
		t.Fatalf("the directory seam does not make the log's parents: %v", err)
	}
}

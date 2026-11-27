package main

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

// fakeLogFile records the line written and fails Write or Close when set.
type fakeLogFile struct {
	written  string
	writeErr error
	closeErr error
}

func (f *fakeLogFile) Write(b []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.written += string(b)
	return len(b), nil
}

func (f *fakeLogFile) Close() error { return f.closeErr }

// fakeLogInfo reports the size the rotation check reads.
type fakeLogInfo struct{ size int64 }

func (fakeLogInfo) Name() string       { return "calls.log" }
func (f fakeLogInfo) Size() int64      { return f.size }
func (fakeLogInfo) Mode() fs.FileMode  { return 0 }
func (fakeLogInfo) ModTime() time.Time { return time.Time{} }
func (fakeLogInfo) IsDir() bool        { return false }
func (fakeLogInfo) Sys() any           { return nil }

// fakeCallLog is a callLog under /log/calls.log whose seams record their calls,
// so a test drives every branch without the filesystem.
type fakeCallLog struct {
	*callLog
	file     *fakeLogFile
	unlocked int
	renamed  string
	madeDir  string
	lockErr  error
}

func newFakeCallLog(size int64) *fakeCallLog {
	f := &fakeCallLog{file: &fakeLogFile{}}
	f.callLog = &callLog{
		path:   "/log/calls.log",
		now:    func() time.Time { return time.Date(2026, 11, 26, 20, 27, 3, 0, time.UTC) },
		stat:   func(string) (fs.FileInfo, error) { return fakeLogInfo{size: size}, nil },
		mkdir:  func(dir string, _ fs.FileMode) error { f.madeDir = dir; return nil },
		rename: func(old, new string) error { f.renamed = old + " -> " + new; return nil },
		open:   func(string) (callLogAppender, error) { return f.file, nil },
		lock: func(string) (func(), error) {
			if f.lockErr != nil {
				return nil, f.lockErr
			}
			return func() { f.unlocked++ }, nil
		},
	}
	return f
}

// TestCallLogLine: append writes the line's fields, and neither a flag nor a
// later argument reaches it.
func TestCallLogLine(t *testing.T) {
	t.Parallel()
	f := newFakeCallLog(0)
	if err := f.append("bd", []string{"create", "bead", "extraword", "--token", "s3cr3t", "later-secret"}, 1, 40*time.Millisecond, "/gt/gastown"); err != nil {
		t.Fatalf("append: %v", err)
	}
	want := "2026-11-26T20:27:03Z\tbd\tcreate bead\t1\t40\t/gt/gastown\n"
	if f.file.written != want {
		t.Errorf("line = %q, want %q", f.file.written, want)
	}
	for _, secret := range []string{"extraword", "--token", "s3cr3t", "later-secret"} {
		if strings.Contains(f.file.written, secret) {
			t.Errorf("line %q holds %q; a flag value or later argument was recorded", f.file.written, secret)
		}
	}
	if f.unlocked != 1 {
		t.Errorf("unlock calls = %d, want 1", f.unlocked)
	}
	if f.renamed != "" {
		t.Errorf("rotated a log of size 0: %q", f.renamed)
	}
}

// TestCallLogSanitisesFields: a newline or tab in the tool or directory cannot
// make a whole line or an extra field.
func TestCallLogSanitisesFields(t *testing.T) {
	t.Parallel()
	f := newFakeCallLog(0)
	if err := f.append("bd\nforged", []string{"create"}, 0, 0, "/gt\tforged\ntown"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if got := strings.Count(f.file.written, "\n"); got != 1 {
		t.Errorf("newlines = %d, want 1 in %q", got, f.file.written)
	}
	if got := strings.Count(f.file.written, "\t"); got != 5 {
		t.Errorf("tabs = %d, want 5 in %q", got, f.file.written)
	}
}

// TestCallLogRotation: a log over 5 MiB is renamed to calls.log.1 before the
// append; a log at the limit or of size 0 is not.
func TestCallLogRotation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		size     int64
		wantMove string
	}{
		{"over 5 MiB", callLogMaxBytes + 1, "/log/calls.log -> /log/calls.log.1"},
		{"at 5 MiB", callLogMaxBytes, ""},
		{"empty", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeCallLog(tc.size)
			if err := f.append("bd", nil, 0, 0, "/d"); err != nil {
				t.Fatalf("append: %v", err)
			}
			if f.renamed != tc.wantMove {
				t.Errorf("rotation = %q, want %q", f.renamed, tc.wantMove)
			}
		})
	}
}

// TestCallLogAbsentFile: a log that does not exist yet is appended without a
// rotation, so the first call in a fresh town logs.
func TestCallLogAbsentFile(t *testing.T) {
	t.Parallel()
	f := newFakeCallLog(0)
	f.stat = func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	f.rename = func(string, string) error { t.Fatal("rotated a missing log"); return nil }
	if err := f.append("bd", nil, 0, 0, "/d"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if f.file.written == "" {
		t.Error("a missing log got no line")
	}
}

// TestCallLogErrors: each file operation that fails is returned, and the lock
// is released on every path past acquisition.
func TestCallLogErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, tc := range []struct {
		name  string
		setup func(*fakeCallLog)
		want  string
		held  bool
	}{
		{"lock", func(f *fakeCallLog) { f.lockErr = boom }, "lock call log", false},
		{"stat", func(f *fakeCallLog) { f.stat = func(string) (fs.FileInfo, error) { return nil, boom } }, "stat call log", true},
		{"rename", func(f *fakeCallLog) { f.rename = func(string, string) error { return boom } }, "rotate call log", true},
		{"open", func(f *fakeCallLog) { f.open = func(string) (callLogAppender, error) { return nil, boom } }, "append call log", true},
		{"write", func(f *fakeCallLog) { f.file.writeErr = boom }, "append call log", true},
		{"close", func(f *fakeCallLog) { f.file.closeErr = boom }, "close call log", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeCallLog(callLogMaxBytes + 1)
			tc.setup(f)
			err := f.append("bd", nil, 0, 0, "/d")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("append error = %v, want one naming %q", err, tc.want)
			}
			if held := f.unlocked > 0; held != tc.held {
				t.Errorf("lock released = %v, want %v", held, tc.held)
			}
		})
	}
}

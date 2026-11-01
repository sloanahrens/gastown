package hostload

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// fakeTail is a tailReader a test scripts: it serves file, or fails the read
// the test names.
type fakeTail struct {
	file    []byte
	statErr error
	readErr error
}

func (f fakeTail) Stat() (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return fakeInfo{size: int64(len(f.file))}, nil
}

func (f fakeTail) ReadAt(p []byte, off int64) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	if off > int64(len(f.file)) {
		return 0, io.EOF
	}
	return copy(p, f.file[off:]), nil
}

// fakeInfo is an os.FileInfo whose size is the only reading a tail needs.
type fakeInfo struct{ size int64 }

func (f fakeInfo) Name() string       { return "daemon.log" }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() os.FileMode  { return 0o644 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return nil }

// TestHostLoadTailDropsThePartialFirstLine checks a tail read starts on a line
// boundary, so the parser never reads half a log line as a whole one.
func TestHostLoadTailDropsThePartialFirstLine(t *testing.T) {
	t.Parallel()
	got, err := tail(fakeTail{file: []byte("0123456789\nabcdef\n")}, 10)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if want := "abcdef\n"; string(got) != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
}

// TestHostLoadTailReadsAFileShorterThanItsBound checks a log shorter than the bound is
// read whole, with no partial line to drop.
func TestHostLoadTailReadsAFileShorterThanItsBound(t *testing.T) {
	t.Parallel()
	got, err := tail(fakeTail{file: []byte("one line\n")}, 1<<20)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if want := "one line\n"; string(got) != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
}

// TestHostLoadTailDropsATailWithNoCompleteLine checks a bound that lands inside one
// long line returns no lines rather than half of one.
func TestHostLoadTailDropsATailWithNoCompleteLine(t *testing.T) {
	t.Parallel()
	got, err := tail(fakeTail{file: []byte("0123456789abcdef")}, 10)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tail = %q, want no lines", got)
	}
}

// TestHostLoadTailRefusesAFailedRead drives the two failures a real file cannot be
// made to produce: a Stat and a ReadAt that fail.
func TestHostLoadTailRefusesAFailedRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		f    fakeTail
		want string
	}{
		{name: "stat", f: fakeTail{statErr: errors.New("stat failed")}, want: "stat failed"},
		{name: "read at", f: fakeTail{file: []byte("x\n"), readErr: errors.New("read failed")}, want: "read failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tail(tc.f, 10)
			if err == nil || err.Error() != tc.want {
				t.Errorf("tail = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestHostLoadReadTailReadsTheEndOfAFile checks the production reader: a real file is
// opened and only its end is read.
func TestHostLoadReadTailReadsTheEndOfAFile(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/daemon.log"
	if err := os.WriteFile(path, []byte("old line\nnew line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTail(path, int64(len("new line\n")))
	if err != nil {
		t.Fatalf("readTail: %v", err)
	}
	if want := "new line\n"; string(got) != want {
		t.Errorf("readTail = %q, want %q", got, want)
	}
}

// TestHostLoadReadTailRefusesAPathItCannotOpen checks a missing log is an error the
// tick probe can report, not an empty log.
func TestHostLoadReadTailRefusesAPathItCannotOpen(t *testing.T) {
	t.Parallel()
	if _, err := readTail(t.TempDir()+"/missing.log", 10); err == nil {
		t.Fatal("readTail read a log that does not exist")
	}
}

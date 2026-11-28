package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errInjected is the failure a faulty copy injects into a write or a close.
var errInjected = errors.New("injected failure")

// planted writes a file of content under a fresh directory and returns its path.
func planted(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// faultyCopy wraps createCopy so the copy it makes fails on its write or on its close. The real file is
// always closed, so the injected failure is the only difference.
func faultyCopy(failWrite, failClose bool) copyCreator {
	return func(name string) (io.WriteCloser, error) {
		f, err := createCopy(name)
		if err != nil {
			return nil, err
		}
		return faultyFile{f, failWrite, failClose}, nil
	}
}

type faultyFile struct {
	io.WriteCloser
	failWrite, failClose bool
}

func (f faultyFile) Write(p []byte) (int, error) {
	if f.failWrite {
		return 0, errInjected
	}
	return f.WriteCloser.Write(p)
}

func (f faultyFile) Close() error {
	err := f.WriteCloser.Close()
	if f.failClose {
		return errInjected
	}
	return err
}

// TestCacheProgStatsCopy: the copy holds the source's bytes, and the source is left as it was.
func TestCacheProgStatsCopy(t *testing.T) {
	t.Parallel()
	content := []byte("{\"hits\":1}\n\x00\xff\xfe tail\n")
	src := planted(t, "stats.jsonl", content)
	dst := filepath.Join(t.TempDir(), "copy.jsonl")

	if err := copyCacheProgStats(src, dst, createCopy); err != nil {
		t.Fatalf("copyCacheProgStats: %v", err)
	}
	if got, err := os.ReadFile(dst); err != nil || !bytes.Equal(got, content) {
		t.Errorf("copy = %q, %v; want %q", got, err, content)
	}
	if got, err := os.ReadFile(src); err != nil || !bytes.Equal(got, content) {
		t.Errorf("source = %q, %v; want it unchanged", got, err)
	}
}

// TestCacheProgStatsExistingCopy: a file already at dst is an error, and that file is left as it was.
func TestCacheProgStatsExistingCopy(t *testing.T) {
	t.Parallel()
	src := planted(t, "stats.jsonl", []byte("new\n"))
	prior := []byte("an earlier copy\n")
	dst := planted(t, "copy.jsonl", prior)

	err := copyCacheProgStats(src, dst, createCopy)
	if !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), dst) {
		t.Fatalf("copyCacheProgStats over an existing file: err = %v; want an exists error naming %s", err, dst)
	}
	if got, rerr := os.ReadFile(dst); rerr != nil || !bytes.Equal(got, prior) {
		t.Errorf("existing file = %q, %v; want it unchanged", got, rerr)
	}
}

// TestCacheProgStatsFailures: a missing source, an unreadable source, a copy that cannot be created, and a
// write or close that fails are each an error naming the path that failed, and none leaves a copy behind.
func TestCacheProgStatsFailures(t *testing.T) {
	t.Parallel()
	src := planted(t, "stats.jsonl", []byte("kept\n"))
	cases := []struct {
		name     string
		src, dst string
		create   copyCreator
		dstNamed bool  // the error names dst; otherwise it names src
		want     error // nil when any error will do
	}{
		{"missing source", filepath.Join(t.TempDir(), "absent.jsonl"), filepath.Join(t.TempDir(), "copy"), createCopy, false, fs.ErrNotExist},
		{"source is a directory", t.TempDir(), filepath.Join(t.TempDir(), "copy"), createCopy, false, nil},
		{"copy cannot be created", src, filepath.Join(t.TempDir(), "no-dir", "copy"), createCopy, true, fs.ErrNotExist},
		{"write fails", src, filepath.Join(t.TempDir(), "copy"), faultyCopy(true, false), true, errInjected},
		{"close fails", src, filepath.Join(t.TempDir(), "copy"), faultyCopy(false, true), true, errInjected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := copyCacheProgStats(tc.src, tc.dst, tc.create)
			if err == nil {
				t.Fatal("copyCacheProgStats returned nil")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v; want it to wrap %v", err, tc.want)
			}
			named := tc.src
			if tc.dstNamed {
				named = tc.dst
			}
			if !strings.Contains(err.Error(), named) {
				t.Errorf("err = %v; want it to name %s", err, named)
			}
			if _, serr := os.Lstat(tc.dst); !os.IsNotExist(serr) {
				t.Errorf("copy %s left behind, Lstat err = %v", tc.dst, serr)
			}
		})
	}
}

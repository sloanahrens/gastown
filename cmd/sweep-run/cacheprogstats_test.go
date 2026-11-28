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

// stuckCopy is a copy whose write fails after it has turned the copy into a non-empty directory, so dropping
// the copy after the failed write cannot remove it.
func stuckCopy() copyCreator {
	return func(name string) (io.WriteCloser, error) {
		f, err := createCopy(name)
		if err != nil {
			return nil, err
		}
		return stuckFile{f, name}, nil
	}
}

type stuckFile struct {
	io.WriteCloser
	dst string
}

func (f stuckFile) Write([]byte) (int, error) {
	if err := os.Remove(f.dst); err != nil {
		return 0, err
	}
	if err := os.Mkdir(f.dst, 0o755); err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(f.dst, "held"), nil, 0o644); err != nil {
		return 0, err
	}
	return 0, errInjected
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

// TestCacheProgStatsDropFails: when a failed write's copy cannot be dropped, the error reports both failures.
func TestCacheProgStatsDropFails(t *testing.T) {
	t.Parallel()
	src := planted(t, "stats.jsonl", []byte("kept\n"))
	dst := filepath.Join(t.TempDir(), "copy")

	err := copyCacheProgStats(src, dst, stuckCopy())
	if !errors.Is(err, errInjected) || !strings.Contains(err.Error(), "dropping the copy "+dst) {
		t.Fatalf("copyCacheProgStats with an undroppable copy: err = %v; want the write failure and a drop error naming %s", err, dst)
	}
}

// statsAt plants the cache program's stats file under root and returns its path.
func statsAt(t *testing.T, root string, content []byte) string {
	t.Helper()
	dir := filepath.Join(root, cacheProgDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, cacheProgStatsName)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCacheProgStatsMove: the source's bytes land under the log directory's copy name and the source is
// gone.
func TestCacheProgStatsMove(t *testing.T) {
	t.Parallel()
	cacheRoot, logDir := t.TempDir(), t.TempDir()
	content := []byte("{\"hits\":1}\n\x00\xff\xfe tail\n")
	src := statsAt(t, cacheRoot, content)

	if err := moveCacheProgStats(cacheRoot, logDir, os.Remove); err != nil {
		t.Fatalf("moveCacheProgStats: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(logDir, cacheProgStatsCopyName)); err != nil || !bytes.Equal(got, content) {
		t.Errorf("copy = %q, %v; want %q", got, err, content)
	}
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Errorf("source %s left behind, Lstat err = %v", src, err)
	}
}

// TestCacheProgStatsNoSource: no stats file is nil, writes no copy, and removes nothing.
func TestCacheProgStatsNoSource(t *testing.T) {
	t.Parallel()
	cacheRoot, logDir := t.TempDir(), t.TempDir()
	var removed []string
	remove := func(name string) error { removed = append(removed, name); return nil }

	if err := moveCacheProgStats(cacheRoot, logDir, remove); err != nil {
		t.Fatalf("moveCacheProgStats with no stats file: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(logDir, cacheProgStatsCopyName)); !os.IsNotExist(err) {
		t.Errorf("no source but a copy was written, Lstat err = %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v; want nothing removed", removed)
	}
}

// TestCacheProgStatsMoveFails: an unreadable source, an earlier copy where the copy goes, and a removal
// that fails are each an error naming the path at fault, and a failed copy removes nothing.
func TestCacheProgStatsMoveFails(t *testing.T) {
	t.Parallel()
	t.Run("source unreadable", func(t *testing.T) {
		t.Parallel()
		cacheRoot, logDir := t.TempDir(), t.TempDir()
		src := filepath.Join(cacheRoot, cacheProgDirName, cacheProgStatsName)
		if err := os.WriteFile(filepath.Join(cacheRoot, cacheProgDirName), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		removed := 0
		err := moveCacheProgStats(cacheRoot, logDir, func(string) error { removed++; return nil })
		if err == nil || !strings.Contains(err.Error(), src) || removed != 0 {
			t.Fatalf("err = %v, removed %d; want an error naming %s and no removal", err, removed, src)
		}
	})
	t.Run("copy exists", func(t *testing.T) {
		t.Parallel()
		cacheRoot, logDir := t.TempDir(), t.TempDir()
		content := []byte("new\n")
		src := statsAt(t, cacheRoot, content)
		dst := filepath.Join(logDir, cacheProgStatsCopyName)
		if err := os.WriteFile(dst, []byte("earlier\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		removed := 0
		err := moveCacheProgStats(cacheRoot, logDir, func(string) error { removed++; return nil })
		if !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), dst) || removed != 0 {
			t.Fatalf("err = %v, removed %d; want an exists error naming %s and no removal", err, removed, dst)
		}
		if got, rerr := os.ReadFile(src); rerr != nil || !bytes.Equal(got, content) {
			t.Errorf("source = %q, %v; want it unchanged", got, rerr)
		}
	})
	t.Run("remove fails", func(t *testing.T) {
		t.Parallel()
		cacheRoot, logDir := t.TempDir(), t.TempDir()
		content := []byte("kept\n")
		src := statsAt(t, cacheRoot, content)
		err := moveCacheProgStats(cacheRoot, logDir, func(name string) error {
			if name == src {
				return errInjected
			}
			return os.Remove(name)
		})
		if !errors.Is(err, errInjected) || !strings.Contains(err.Error(), src) {
			t.Fatalf("err = %v; want it to wrap %v and name %s", err, errInjected, src)
		}
		if _, serr := os.Lstat(filepath.Join(logDir, cacheProgStatsCopyName)); !os.IsNotExist(serr) {
			t.Errorf("copy left behind after a failed source removal, Lstat err = %v", serr)
		}
		if got, rerr := os.ReadFile(src); rerr != nil || !bytes.Equal(got, content) {
			t.Errorf("source = %q, %v; want it kept", got, rerr)
		}
	})
	t.Run("drop fails", func(t *testing.T) {
		t.Parallel()
		cacheRoot, logDir := t.TempDir(), t.TempDir()
		src := statsAt(t, cacheRoot, []byte("kept\n"))
		dst := filepath.Join(logDir, cacheProgStatsCopyName)
		errHeld := errors.New("copy held")
		err := moveCacheProgStats(cacheRoot, logDir, func(name string) error {
			if name == src {
				return errInjected
			}
			return errHeld
		})
		if !errors.Is(err, errInjected) || !strings.Contains(err.Error(), src) {
			t.Fatalf("err = %v; want the removal failure wrapping %v and naming %s", err, errInjected, src)
		}
		if !errors.Is(err, errHeld) || !strings.Contains(err.Error(), "dropping the copy "+dst) {
			t.Errorf("err = %v; want the drop failure wrapping %v and naming %s", err, errHeld, dst)
		}
	})
}

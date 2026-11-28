package main

import (
	"bytes"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

func TestTreeDirMakesTheSweepBinDirectoryAndRemovesExactlyIt(t *testing.T) {
	t.Parallel()
	const made = "/fake/sweep-bin.1"
	var gotDir, gotPattern string
	var removed []string
	ts := treeSweep{
		MkdirTemp: func(dir, pattern string) (string, error) {
			gotDir, gotPattern = dir, pattern
			return made, nil
		},
		RemoveAll: func(path string) error {
			removed = append(removed, path)
			return nil
		},
	}
	var stderr bytes.Buffer
	dir, remove, ok := ts.tempDir(&stderr)
	if !ok {
		t.Fatalf("tempDir ok = false, stderr %q", stderr.String())
	}
	if gotDir != "" || gotPattern != "sweep-bin." {
		t.Errorf("MkdirTemp(%q, %q), want the system temp directory and the pattern sweep-bin.", gotDir, gotPattern)
	}
	if dir != made {
		t.Errorf("dir = %q, want %q", dir, made)
	}
	remove()
	if !slices.Equal(removed, []string{made}) {
		t.Errorf("RemoveAll paths = %q, want exactly %q", removed, made)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want none on success", stderr.String())
	}
}

func TestTreeDirRefusesWhenMkdirTempFailsAndRemovesNothing(t *testing.T) {
	t.Parallel()
	mkErr := errors.New("no space left on device")
	removeCalls := 0
	ts := treeSweep{
		MkdirTemp: func(string, string) (string, error) { return "", mkErr },
		RemoveAll: func(string) error {
			removeCalls++
			return nil
		},
	}
	var stderr bytes.Buffer
	dir, remove, ok := ts.tempDir(&stderr)
	if ok {
		t.Fatal("tempDir ok = true after a failed MkdirTemp, want false")
	}
	if dir != "" {
		t.Errorf("dir = %q after a failed MkdirTemp, want empty", dir)
	}
	if remove == nil {
		t.Fatal("remove is nil after a failed MkdirTemp, want a no-op the caller can defer")
	}
	remove()
	if removeCalls != 0 {
		t.Errorf("RemoveAll ran %d times after a failed MkdirTemp, want none", removeCalls)
	}
	line := stderr.String()
	if !strings.HasPrefix(line, "sweep-run: ") || !strings.Contains(line, mkErr.Error()) || strings.Count(line, "\n") != 1 {
		t.Errorf("stderr = %q, want one sweep-run: line naming the error %q", line, mkErr)
	}
}

func TestTreeDirReportsAFailedRemovalInOneLineNamingTheDirectory(t *testing.T) {
	t.Parallel()
	const made = "/fake/sweep-bin.2"
	ts := treeSweep{
		MkdirTemp: func(string, string) (string, error) { return made, nil },
		RemoveAll: func(string) error { return fs.ErrPermission },
	}
	var stderr bytes.Buffer
	_, remove, ok := ts.tempDir(&stderr)
	if !ok {
		t.Fatalf("tempDir ok = false, stderr %q", stderr.String())
	}
	remove()
	line := stderr.String()
	if !strings.HasPrefix(line, "sweep-run: ") || !strings.Contains(line, made) || strings.Count(line, "\n") != 1 {
		t.Errorf("stderr = %q, want one sweep-run: line naming the directory %q", line, made)
	}
	if !strings.Contains(line, fs.ErrPermission.Error()) {
		t.Errorf("stderr = %q, want the removal error %q in it", line, fs.ErrPermission)
	}
}

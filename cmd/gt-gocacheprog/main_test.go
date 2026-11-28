package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// checkRefused pins one refused run: 1, empty stdout, one stderr line holding want (gt-6i1x13).
func checkRefused(t *testing.T, args []string, check func(string) error, want string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr, check); code != 1 || stdout.Len() != 0 {
		t.Fatalf("run = %d, stdout %q: want 1 and empty", code, stdout.String())
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, want) {
		t.Errorf("stderr = %q, want one line containing %q", got, want)
	}
}

// TestRunRefusesBadArgs: a missing, relative or unusable -dir and an unknown
// flag each exit 1 with one cause line (gt-6i1x13).
func TestRunRefusesBadArgs(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"missing":      {nil, "-dir is required"},
		"relative":     {[]string{"-dir", "cache"}, "not absolute"},
		"unusable":     {[]string{"-dir", filepath.Join(file, "store")}, "store"},
		"unknown flag": {[]string{"-bogus"}, "flag provided but not defined"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkRefused(t, tc.args, func(string) error { return nil }, tc.want)
		})
	}
}

// TestRunGoodDirIsSilentAndEmpty: a usable -dir exits 0 with both streams
// empty and the directory empty afterwards (gt-6i1x13).
func TestRunGoodDirIsSilentAndEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-dir", dir}, &stdout, &stderr, writableCheck); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("run = %d, stdout %q, stderr %q: want 0 and nothing", code, stdout.String(), stderr.String())
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("dir = %v, %v: want empty", entries, err)
	}
}

// TestRunFailingCheck: a writability check that refuses exits 1 with its cause
// on stderr (gt-6i1x13).
func TestRunFailingCheck(t *testing.T) {
	t.Parallel()
	cause := errors.New("disk is read-only")
	checkRefused(t, []string{"-dir", t.TempDir()}, func(string) error { return cause }, cause.Error())
}

// TestProbeWritable: a failed create, close or remove refuses the directory by
// name (gt-6i1x13).
func TestProbeWritable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		create func(string, string) (*os.File, error)
		remove func(string) error
		want   string
	}{
		"create fails": {func(string, string) (*os.File, error) { return nil, errors.New("create refused") }, os.Remove, "create refused"},
		"close fails":  {func(string, string) (*os.File, error) { return closed, nil }, os.Remove, "file already closed"},
		"remove fails": {os.CreateTemp, func(string) error { return errors.New("remove refused") }, "remove refused"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := probeWritable(dir, tc.create, tc.remove); err == nil || !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("probeWritable = %v, want an error naming %q and %q", err, dir, tc.want)
			}
		})
	}
}

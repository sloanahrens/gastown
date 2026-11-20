package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// at is the fixed reading time; UTC keeps the day file's name and the line's
// stamp in step.
var at = time.Date(2026, 11, 20, 16, 24, 0, 0, time.UTC)

// read answers the two sysctl questions with load and ncpu.
func read(load, ncpu string) func(string, ...string) (string, error) {
	return func(_ string, args ...string) (string, error) {
		if args[1] == "hw.ncpu" {
			return ncpu, nil
		}
		return load, nil
	}
}

// TestSysctlRefusesACommandThatCannotStart covers the exec error return: a
// path that cannot exist fails before any process starts (gt-emjwp9).
func TestSysctlRefusesACommandThatCannotStart(t *testing.T) {
	t.Parallel()
	if out, err := sysctl("/nonexistent-gt-hostload-sysctl", "-n", "vm.loadavg"); err == nil || out != "" {
		t.Errorf("sysctl = %q, %v: want an error and no output", out, err)
	}
}

// TestHostLoadRunWritesOneLine: the two sysctl answers become one line in the
// 20th's file under --dir, and a clean run prints nothing (gt-emjwp9).
func TestHostLoadRunWritesOneLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var stderr bytes.Buffer
	if code := run([]string{"--dir", dir}, &stderr, func() time.Time { return at }, read("{ 21.53 22.70 21.14 }\n", "24\n")); code != 0 || stderr.Len() != 0 {
		t.Fatalf("run = %d, stderr %q: want 0 and nothing", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "2026-11-20.log"))
	want := "2026-11-20T16:24:00Z load1=21.53 load5=22.70 load15=21.14 ncpu=24\n"
	if err != nil || string(data) != want {
		t.Errorf("2026-11-20.log = %q, %v: want %q", data, err, want)
	}
}

// failsOne answers both sysctl questions but fails the named one (gt-phge44).
func failsOne(name string) func(string, ...string) (string, error) {
	ok := read("{ 21.53 22.70 21.14 }", "24")
	return func(n string, args ...string) (string, error) {
		if args[1] == name {
			return "", errors.New("sysctl: read failed")
		}
		return ok(n, args...)
	}
}

// checkLoudFailure pins a failed read: 1, the step in one stderr line, no file (gt-phge44).
func checkLoudFailure(t *testing.T, step string) {
	t.Helper()
	dir := t.TempDir()
	var stderr bytes.Buffer
	if code := run([]string{"--dir", dir}, &stderr, func() time.Time { return at }, failsOne(step)); code != 1 {
		t.Errorf("run = %d, want 1", code)
	}
	if want := "gt-hostload: " + step + ": "; !strings.HasPrefix(stderr.String(), want) || strings.Count(stderr.String(), "\n") != 1 {
		t.Errorf("stderr = %q, want one line starting %q", stderr.String(), want)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("dir = %v, %v: want empty, so no day file", entries, err)
	}
}

// TestHostLoadRunFailsLoudOnLoad: a failing vm.loadavg read returns 1, names the step, writes no file (gt-phge44).
func TestHostLoadRunFailsLoudOnLoad(t *testing.T) {
	t.Parallel()
	checkLoudFailure(t, "vm.loadavg")
}

// TestHostLoadRunFailsLoudOnNcpu: a failing hw.ncpu read after a good load read returns 1, names the step, writes no file (gt-phge44).
func TestHostLoadRunFailsLoudOnNcpu(t *testing.T) {
	t.Parallel()
	checkLoudFailure(t, "hw.ncpu")
}

// TestHostLoadRunRefuses: each failing step names itself in one stderr line,
// returns 1, and leaves no day file behind (gt-emjwp9).
func TestHostLoadRunRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	bad := func(string, ...string) (string, error) { return "", boom }
	for _, c := range []struct {
		step string
		args []string
		read func(string, ...string) (string, error)
	}{
		{"flags", []string{"--nope"}, read("{ 1 2 3 }", "2")},
		{"vm.loadavg", []string{"--dir", dir}, bad},
		{"vm.loadavg", []string{"--dir", dir}, read("nonsense", "2")},
		{"hw.ncpu", []string{"--dir", dir}, func(_ string, args ...string) (string, error) {
			if args[1] == "hw.ncpu" {
				return "", boom
			}
			return "{ 1 2 3 }", nil
		}},
		{"hw.ncpu", []string{"--dir", dir}, read("{ 1 2 3 }", "many")},
		{"append", []string{"--dir", file}, read("{ 1 2 3 }", "2")},
	} {
		var stderr bytes.Buffer
		if code := run(c.args, &stderr, func() time.Time { return at }, c.read); code != 1 {
			t.Errorf("%s: run = %d, want 1", c.step, code)
		}
		if want := "gt-hostload: " + c.step + ": "; !strings.HasPrefix(stderr.String(), want) || strings.Count(stderr.String(), "\n") != 1 {
			t.Errorf("%s: stderr = %q, want one line starting %q", c.step, stderr.String(), want)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("dir = %v, %v: want only a-file, so no partial day file", entries, err)
	}
}

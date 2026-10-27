package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestRunnerRunsTheHourAsGtMetricsRun holds the entry point to what the daemon
// records: the program it execs, the argv and environment the exec carries, and
// the status a start that never happens returns (gt-98pgy).
func TestRunnerRunsTheHourAsGtMetricsRun(t *testing.T) {
	t.Parallel()

	env := []string{"GT_TOWN_ROOT=/town", "GT_PLUGIN_NAME=town-metrics"}

	t.Run("the hour runs as gt metrics run", func(t *testing.T) {
		t.Parallel()
		var gotPath string
		var gotArgv, gotEnv []string
		r := runner{
			lookPath: func(name string) (string, error) {
				if name != "gt" {
					t.Errorf("lookPath(%q), want gt", name)
				}
				return "/usr/local/bin/gt", nil
			},
			exec: func(path string, argv, e []string) error {
				gotPath, gotArgv, gotEnv = path, argv, e
				return nil
			},
		}
		var stderr bytes.Buffer
		if code := r.run(env, &stderr); code != 0 {
			t.Fatalf("run() = %d, want 0; stderr: %s", code, stderr.String())
		}
		if gotPath != "/usr/local/bin/gt" {
			t.Errorf("exec path = %q, want the gt lookPath found", gotPath)
		}
		if want := []string{"gt", "metrics", "run"}; !slices.Equal(gotArgv, want) {
			t.Errorf("exec argv = %q, want %q", gotArgv, want)
		}
		if !slices.Equal(gotEnv, env) {
			t.Errorf("exec env = %q, want the environment the runner passed (%q)", gotEnv, env)
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want nothing written on a run that started", stderr.String())
		}
	})

	t.Run("gt missing from PATH fails the hour", func(t *testing.T) {
		t.Parallel()
		execCalled := false
		r := runner{
			lookPath: func(string) (string, error) {
				return "", errors.New(`exec: "gt": executable file not found in $PATH`)
			},
			exec: func(string, []string, []string) error {
				execCalled = true
				return nil
			},
		}
		var stderr bytes.Buffer
		if code := r.run(env, &stderr); code != 1 {
			t.Errorf("run() = %d, want 1", code)
		}
		if execCalled {
			t.Error("exec ran without a gt path; the run must fail before it tries")
		}
		if got := stderr.String(); !strings.Contains(got, "cannot find gt") {
			t.Errorf("stderr = %q, want a line naming the missing gt", got)
		}
	})

	t.Run("an exec that fails returns a failed run", func(t *testing.T) {
		t.Parallel()
		r := runner{
			lookPath: func(string) (string, error) { return "/usr/local/bin/gt", nil },
			exec: func(string, []string, []string) error {
				return errors.New("permission denied")
			},
		}
		var stderr bytes.Buffer
		if code := r.run(env, &stderr); code != 1 {
			t.Errorf("run() = %d, want 1", code)
		}
		got := stderr.String()
		if !strings.Contains(got, "/usr/local/bin/gt") || !strings.Contains(got, "permission denied") {
			t.Errorf("stderr = %q, want a line naming the program and the error", got)
		}
	})
}

package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// okTown is the town seam of a test about the exec: it resolves a town the
// environment agrees with, so the run gets past placement.
func okTown() (string, error) {
	return resolveTown(func() (string, error) { return "/town", nil }, func(string) string { return "/town" })
}

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
			townRoot: okTown,
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
			townRoot: okTown,
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
			townRoot: okTown,
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

// TestRunnerRefusesAnHourItCannotPlace pins the placement the hour's record
// depends on: the plugin's directory and GT_TOWN_ROOT name one town, or the run
// fails before gt resolves a town this program never checked (gt-bfrzq).
func TestRunnerRefusesAnHourItCannotPlace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		resolve  func() (string, error)
		fromEnv  string
		wantCode int
		wantMsg  string
	}{
		{
			name:    "the directory and GT_TOWN_ROOT name one town",
			resolve: func() (string, error) { return "/town", nil },
			fromEnv: "/town",
		},
		{
			name:    "a hand run inside the town names no GT_TOWN_ROOT",
			resolve: func() (string, error) { return "/town", nil },
		},
		{
			name:    "a trailing slash names the same town",
			resolve: func() (string, error) { return "/town", nil },
			fromEnv: "/town/",
		},
		{
			name:     "the directory and GT_TOWN_ROOT name different towns",
			resolve:  func() (string, error) { return "/town", nil },
			fromEnv:  "/elsewhere",
			wantCode: 1,
			wantMsg:  `"/elsewhere"`,
		},
		{
			name: "neither names a town",
			resolve: func() (string, error) {
				return "", errors.New("not in a Gas Town workspace")
			},
			wantCode: 1,
			wantMsg:  "not in a Gas Town workspace",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			execCalled := false
			r := runner{
				lookPath: func(string) (string, error) { return "/usr/local/bin/gt", nil },
				exec: func(string, []string, []string) error {
					execCalled = true
					return nil
				},
				townRoot: func() (string, error) {
					return resolveTown(tc.resolve, func(key string) string {
						if key != "GT_TOWN_ROOT" {
							t.Errorf("getenv(%q), want GT_TOWN_ROOT", key)
						}
						return tc.fromEnv
					})
				},
			}
			var stderr bytes.Buffer
			if code := r.run(nil, &stderr); code != tc.wantCode {
				t.Fatalf("run() = %d, want %d; stderr: %s", code, tc.wantCode, stderr.String())
			}
			if wantExec := tc.wantCode == 0; execCalled != wantExec {
				t.Errorf("exec ran = %v, want %v", execCalled, wantExec)
			}
			if tc.wantMsg != "" && !strings.Contains(stderr.String(), tc.wantMsg) {
				t.Errorf("stderr = %q, want it to name %q", stderr.String(), tc.wantMsg)
			}
			if tc.wantCode == 0 && stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing written on a run that started", stderr.String())
			}
		})
	}
}

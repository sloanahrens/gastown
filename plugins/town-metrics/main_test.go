package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/workspace"
)

// fixtureTown lays out the directories production has: a town holding the
// mayor/town.json marker, and the plugin directory this program runs in. It
// returns the town root, not the plugin directory.
func fixtureTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	writeDir(t, filepath.Join(town, "mayor"))
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("writing the town marker: %v", err)
	}
	writeDir(t, filepath.Join(town, "plugins", "town-metrics"))
	return town
}

func writeDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
}

// inTown resolves the town from a route to it the way production does: the
// plugin directory under that route, walked by workspace.Find.
func inTown(route string) func() (string, error) {
	return func() (string, error) {
		return workspace.Find(filepath.Join(route, "plugins", "town-metrics"))
	}
}

// TestRunnerRunsTheHourAsGtMetricsRun holds the entry point to what the daemon
// records: the program it execs, the argv and environment the exec carries, and
// the status a start that never happens returns (gt-98pgy).
func TestRunnerRunsTheHourAsGtMetricsRun(t *testing.T) {
	t.Parallel()

	town := fixtureTown(t)
	townRoot := inTown(town)
	env := []string{"GT_TOWN_ROOT=" + town, "GT_PLUGIN_NAME=town-metrics"}

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
			townRoot: townRoot,
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
			townRoot: townRoot,
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
			townRoot: townRoot,
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
// depends on: the town the plugin directory resolves to and GT_TOWN_ROOT name
// one town, or the run fails before gt resolves a town this program never
// checked (gt-bfrzq). The resolver is the production one, walked over a town
// that exists on disk, so a route the test accepts is a route the program
// accepts (gt-uzi83).
func TestRunnerRefusesAnHourItCannotPlace(t *testing.T) {
	t.Parallel()

	town := fixtureTown(t)
	elsewhere := fixtureTown(t)
	link := filepath.Join(t.TempDir(), "town-by-another-route")
	if err := os.Symlink(town, link); err != nil {
		t.Fatalf("linking the town: %v", err)
	}

	tests := []struct {
		name     string
		resolve  func() (string, error)
		fromEnv  string
		absErr   error
		wantCode int
		wantMsg  string
	}{
		{
			name:    "the plugin directory and GT_TOWN_ROOT name one town",
			resolve: inTown(town),
			fromEnv: town,
		},
		{
			name:    "a hand run inside the town names no GT_TOWN_ROOT",
			resolve: inTown(town),
		},
		{
			name:    "a trailing slash names the same town",
			resolve: inTown(town),
			fromEnv: town + string(filepath.Separator),
		},
		{
			name:    "GT_TOWN_ROOT reaches the town through a symlink",
			resolve: inTown(town),
			fromEnv: link,
		},
		{
			name:    "the plugin directory reaches the town through a symlink",
			resolve: inTown(link),
			fromEnv: town,
		},
		{
			name:     "two towns are refused",
			resolve:  inTown(town),
			fromEnv:  elsewhere,
			wantCode: 1,
			wantMsg:  elsewhere,
		},
		{
			name:     "a GT_TOWN_ROOT that names no directory is refused",
			resolve:  inTown(town),
			fromEnv:  filepath.Join(t.TempDir(), "no-such-town"),
			wantCode: 1,
			wantMsg:  "no-such-town",
		},
		{
			name: "neither names a town",
			resolve: func() (string, error) {
				return "", errors.New("not in a Gas Town workspace")
			},
			wantCode: 1,
			wantMsg:  "not in a Gas Town workspace",
		},
		{
			name:     "GT_TOWN_ROOT cannot be made absolute",
			resolve:  inTown(town),
			fromEnv:  ".",
			absErr:   errors.New("getting current directory"),
			wantCode: 1,
			wantMsg:  "getting current directory",
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
					abs := filepath.Abs
					if tc.absErr != nil {
						abs = func(string) (string, error) { return "", tc.absErr }
					}
					return resolveTown(tc.resolve, func(key string) string {
						if key != "GT_TOWN_ROOT" {
							t.Errorf("getenv(%q), want GT_TOWN_ROOT", key)
						}
						return tc.fromEnv
					}, abs)
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

package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejocut"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// noEnv is the empty environment: every default these tests read is one no
// variable moved.
func noEnv(string) string { return "" }

// homeEnv is the environment with only HOME set, so a command line reaches the
// town's default.
func homeEnv(key string) string {
	if key == "HOME" {
		return "/home/test"
	}
	return ""
}

// stubGit reports the GitHub origin every test rig names.
type stubGit struct{}

func (stubGit) OriginURL(string) (string, error) {
	return "https://github.com/acme/gastown.git", nil
}

// stubEnv is the seam set a valid command line parses against: every path is a
// directory and a file, so no test touches the real filesystem.
func stubEnv(getenv func(string) string) forgejocut.Env {
	return forgejocut.Env{
		Getenv:     getenv,
		IsDir:      func(string) bool { return true },
		IsFile:     func(string) bool { return true },
		Town:       func() string { return "" },
		Executable: func() (string, error) { return "/repo/forgejo-cutover", nil },
		Git:        stubGit{},
	}
}

// TestMainUsageExitsTwoAndHelpExitsZero holds the script's usage_die and its -h: a
// refused command line exits 2 with the problem named and the usage hint, and the
// help request exits 0 with the flag list on stdout and nothing on stderr.
func TestMainUsageExitsTwoAndHelpExitsZero(t *testing.T) {
	t.Parallel()
	refusals := map[string]struct {
		args   []string
		stderr string
	}{
		"unknown option":             {args: []string{"gastown", "--repo", "acme/gastown", "--nope"}, stderr: "unknown option: --nope"},
		"the dropped --mirror":       {args: []string{"gastown", "--repo", "acme/gastown", "--mirror"}, stderr: "unknown option: --mirror"},
		"no rig":                     {args: []string{"--repo", "acme/gastown"}, stderr: "<rig> is required"},
		"no repository":              {args: []string{"gastown"}, stderr: "--repo is required"},
		"repository with no /":       {args: []string{"gastown", "--repo", "gastown"}, stderr: "--repo 'gastown' must be OWNER/NAME"},
		"api-url without http":       {args: []string{"gastown", "--repo", "acme/gastown", "--api-url", "forgejo.test"}, stderr: "--api-url must be an http:// or https:// URL with a host and no credentials"},
		"repository with a dot":      {args: []string{"gastown", "--repo", "acme/.."}, stderr: "--repo 'acme/..' must be OWNER/NAME"},
		"main-branch with space":     {args: []string{"gastown", "--repo", "acme/gastown", "--main-branch", "ma in"}, stderr: "--main-branch 'ma in' must match"},
		"main-branch with a dash":    {args: []string{"gastown", "--repo", "acme/gastown", "--main-branch", "-x"}, stderr: "--main-branch '-x' is not a branch name"},
		"main-branch with a dot-dot": {args: []string{"gastown", "--repo", "acme/gastown", "--main-branch", "a..b"}, stderr: "--main-branch 'a..b' is not a branch name"},
		"api-url with a password":    {args: []string{"gastown", "--repo", "acme/gastown", "--api-url", "https://user:hunter2@forgejo.test/api/v1"}, stderr: "--api-url must be an http:// or https:// URL with a host and no credentials"},
	}
	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run(tc.args, stubEnv(noEnv), &stdout, &stderr); got != forgejocut.ExitUsage {
				t.Fatalf("run(%v) = %d, want 2 (stderr: %s)", tc.args, got, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("run(%v) stderr = %q, want it to hold %q", tc.args, stderr.String(), tc.stderr)
			}
			if !strings.Contains(stderr.String(), "usage: forgejo-cutover --help") {
				t.Errorf("run(%v) stderr = %q, want the usage hint", tc.args, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("run(%v) stdout = %q, want nothing on a refusal", tc.args, stdout.String())
			}
		})
	}

	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run([]string{flag}, stubEnv(noEnv), &stdout, &stderr); got != forgejocut.ExitOK {
				t.Fatalf("run(%s) = %d, want 0 (stderr: %s)", flag, got, stderr.String())
			}
			// Each of these is read from the constant that holds the default, so a
			// default the text no longer names fails here.
			for _, want := range []string{
				"Usage: forgejo-cutover <rig> --repo OWNER/NAME [options]",
				"--accept-diverged",
				"(default: " + forgejocut.DefaultMainBranch + ")",
				"else " + forgejocut.DefaultAPIURL + ")",
				"(default: " + forgejocut.DefaultProbe + ")",
			} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("run(%s) stdout = %q, want it to hold %q", flag, stdout.String(), want)
				}
			}
			if strings.Contains(stdout.String(), "--mirror") {
				t.Errorf("run(%s) stdout names --mirror, which the port dropped", flag)
			}
			if stderr.Len() != 0 {
				t.Errorf("run(%s) stderr = %q, want nothing on a help request", flag, stderr.String())
			}
		})
	}
}

// TestMainValidCommandLineIsNotYetRunExitsOne: a command line that parses reaches
// the run, which is not ported, so the command exits 1 and says where the cutover
// still runs. The usage hint is left out, since the command line was valid.
func TestMainValidCommandLineIsNotYetRunExitsOne(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	got := run([]string{"gastown", "--repo", "acme/gastown"}, stubEnv(homeEnv), &stdout, &stderr)
	if got != forgejocut.ExitFailure {
		t.Fatalf("run = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not ported yet") || !strings.Contains(stderr.String(), "scripts/forgejo-cutover.sh") {
		t.Errorf("stderr = %q, want the port named and the script that still runs", stderr.String())
	}
	if strings.Contains(stderr.String(), "usage: forgejo-cutover --help") {
		t.Errorf("stderr = %q, want no usage hint: the command line was valid", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing from a run that did not start", stdout.String())
	}
}

// TestMainFailedRigExitsOne: a rig whose directory is missing is a failed run, not
// a refused command line, so it exits 1 with no usage hint.
func TestMainFailedRigExitsOne(t *testing.T) {
	t.Parallel()
	env := stubEnv(homeEnv)
	env.IsDir = func(path string) bool { return path == "/home/test/gt" }
	var stdout, stderr strings.Builder
	got := run([]string{"absent", "--repo", "acme/gastown"}, env, &stdout, &stderr)
	if got != forgejocut.ExitFailure {
		t.Fatalf("run = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no rig directory") {
		t.Errorf("stderr = %q, want the missing rig named", stderr.String())
	}
	if strings.Contains(stderr.String(), "usage: forgejo-cutover --help") {
		t.Errorf("stderr = %q, want no usage hint for a failed run", stderr.String())
	}
}

// TestRunParseWithoutOptionsExitsOne: a parse seam that returns neither options nor
// an error has no command line to run. It exits 1 and says so; reading the missing
// options would panic instead of reporting the failed run.
func TestRunParseWithoutOptionsExitsOne(t *testing.T) {
	t.Parallel()
	empty := func([]string, forgejocut.Env) (*forgejocut.Options, error) {
		return nil, nil
	}
	var stdout, stderr strings.Builder
	if got := runParse(nil, stubEnv(noEnv), &stdout, &stderr, empty); got != forgejocut.ExitFailure {
		t.Fatalf("runParse = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "parsed to no options") {
		t.Errorf("stderr = %q, want the missing options named", stderr.String())
	}
}

// TestRunParseFailedParseIsAFailedRun: a parse error that is not a usage error
// exits 1 and names the error, with no usage hint.
func TestRunParseFailedParseIsAFailedRun(t *testing.T) {
	t.Parallel()
	failing := func([]string, forgejocut.Env) (*forgejocut.Options, error) {
		return nil, errors.New("no origin URL")
	}
	var stdout, stderr strings.Builder
	if got := runParse(nil, stubEnv(noEnv), &stdout, &stderr, failing); got != forgejocut.ExitFailure {
		t.Fatalf("runParse = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "no origin URL") || strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr = %q, want the error named and no usage hint", stderr.String())
	}
}

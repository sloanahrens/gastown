package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejoprobe"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// noEnv is the empty environment: every default these tests read is one no
// variable moved.
func noEnv(string) string { return "" }

// gitFound and gitMissing are the PATH seam at both ends: the host that can
// push, and the one the probe refuses to start on.
func gitFound(file string) (string, error) { return "/usr/bin/" + file, nil }
func gitMissing(string) (string, error)    { return "", exec.ErrNotFound }

// validArgs is the shortest command line the probe accepts.
var validArgs = []string{"rig", "--repo", "acme/rig"}

// TestMainUsageAndHelp holds the two ends of the usage answer: the help request
// prints the flag list and exits 0, and a host without git exits 1 rather than
// letting the run start.
func TestMainUsageAndHelp(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run([]string{flag}, noEnv, gitFound, &stdout, &stderr); got != forgejoprobe.ExitOK {
				t.Fatalf("run(%s) = %d, want 0 (stderr: %s)", flag, got, stderr.String())
			}
			// Each of these is read from the constant that holds the default, so
			// a default the text no longer names fails here.
			for _, want := range []string{
				"Usage: forgejo-probe <rig> --repo OWNER/NAME",
				"--api-url",
				"else " + forgejoprobe.DefaultAPIURL,
				"--repo OWNER/NAME",
				"--town-root",
				"$GT_TOWN_ROOT",
				"--dry-run",
				"(default: " + forgejoprobe.DefaultMainBranch + ")",
				"(default: 1200)",
				"(default: 15)",
			} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("run(%s) stdout = %q, want it to hold %q", flag, stdout.String(), want)
				}
			}
			if stderr.Len() != 0 {
				t.Errorf("run(%s) stderr = %q, want nothing on a help request", flag, stderr.String())
			}
		})
	}

	t.Run("no git", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr strings.Builder
		if got := run(validArgs, noEnv, gitMissing, &stdout, &stderr); got != forgejoprobe.ExitFailure {
			t.Fatalf("run with no git = %d, want 1 (stderr: %s)", got, stderr.String())
		}
		if !strings.Contains(stderr.String(), "forgejo-probe: git is not on PATH") {
			t.Errorf("stderr = %q, want the missing git named", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want nothing on a failed start", stdout.String())
		}
	})
}

// TestMainUsageExitTwo holds the script's usage_die: the message names the
// flag, the usage hint follows it, and the code is 2.
func TestMainUsageExitTwo(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args   []string
		stderr string
	}{
		"unknown option":                     {args: []string{"--nope"}, stderr: "unknown option: --nope"},
		"missing rig":                        {args: []string{"--repo", "acme/rig"}, stderr: "<rig> is required"},
		"--repo without OWNER/NAME":          {args: []string{"rig", "--repo", "gastown"}, stderr: "--repo 'gastown' must be OWNER/NAME"},
		"a timeout that is not a number":     {args: []string{"rig", "--repo", "acme/rig", "--timeout", "soon"}, stderr: "--timeout 'soon' must be a positive integer"},
		"a value that reads as a git option": {args: []string{"rig", "--repo", "acme/rig", "--git-url", "--upload-pack=x"}, stderr: "--git-url '--upload-pack=x' must not start with '-'"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run(tc.args, noEnv, gitFound, &stdout, &stderr); got != forgejoprobe.ExitUsage {
				t.Fatalf("run(%v) = %d, want 2 (stderr: %s)", tc.args, got, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("run(%v) stderr = %q, want it to hold %q", tc.args, stderr.String(), tc.stderr)
			}
			if !strings.Contains(stderr.String(), "usage: forgejo-probe --help") {
				t.Errorf("run(%v) stderr = %q, want the usage hint", tc.args, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("run(%v) stdout = %q, want nothing on a refusal", tc.args, stdout.String())
			}
		})
	}
}

// TestMainUsageNotWired: a command line that passes every check must not exit 0
// as though the gate had passed, since the run that probes arrives in
// gt-tpg0p.92.
func TestMainUsageNotWired(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	if got := run(validArgs, noEnv, gitFound, &stdout, &stderr); got != forgejoprobe.ExitNotWired {
		t.Fatalf("run(%v) = %d, want %d (stderr: %s)", validArgs, got, forgejoprobe.ExitNotWired, stderr.String())
	}
	if got := strings.TrimSpace(stderr.String()); got != notWired {
		t.Errorf("stderr = %q, want %q", got, notWired)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing before the run lands", stdout.String())
	}
}

// TestMainUsageParseFailure: a parse failure that is not a usage error is a
// failed run (1) with its own line, not a refused command line (2) with the
// usage hint. Parse reports no such failure today; the seam stands so a later
// bead reading the admin token file at parse time cannot turn its failure into
// a usage refusal.
func TestMainUsageParseFailure(t *testing.T) {
	t.Parallel()
	unreadable := func([]string, func(string) string) (*forgejoprobe.Options, error) {
		return nil, errors.New("the admin token file is unreadable")
	}
	var stdout, stderr strings.Builder
	if got := runParse(nil, noEnv, gitFound, &stdout, &stderr, unreadable); got != forgejoprobe.ExitFailure {
		t.Fatalf("runParse = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "the admin token file is unreadable") {
		t.Errorf("stderr = %q, want the parse failure named", stderr.String())
	}
	if strings.Contains(stderr.String(), "usage: forgejo-probe --help") {
		t.Errorf("stderr = %q, want no usage hint: this is not a refused command line", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing on a failed run", stdout.String())
	}
}

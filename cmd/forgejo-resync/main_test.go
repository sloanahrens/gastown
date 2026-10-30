package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejoresync"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// noEnv is the empty environment: every default these tests read is one no
// variable moved.
func noEnv(string) string { return "" }

// homeEnv is the environment with only HOME set, for the command lines that must
// reach the backup directory's default.
func homeEnv(key string) string {
	if key == "HOME" {
		return "/home/test"
	}
	return ""
}

// TestMainUsageExitsTwoAndHelpExitsZero holds the script's usage_die and its
// -h: a refused command line exits 2 with the problem named and the usage hint,
// and the help request exits 0 with the flag list on stdout and nothing on
// stderr.
func TestMainUsageExitsTwoAndHelpExitsZero(t *testing.T) {
	t.Parallel()
	refusals := map[string]struct {
		args   []string
		stderr string
	}{
		"unknown option":         {args: []string{"--nope", "acme/gastown"}, stderr: "unknown option: --nope"},
		"no repository":          {args: []string{"--dry-run"}, stderr: "OWNER/NAME is required"},
		"repository with slash":  {args: []string{"gastown"}, stderr: "'gastown' must be OWNER/NAME"},
		"api-url without http":   {args: []string{"--api-url", "forgejo.test", "acme/gastown"}, stderr: "--api-url must be http:// or https://"},
		"main-branch with space": {args: []string{"--main-branch", "ma in", "acme/gastown"}, stderr: "--main-branch 'ma in' must match"},
		"repository with space":  {args: []string{"acme/git town"}, stderr: "'acme/git town' must be OWNER/NAME"},
		"three-part repository":  {args: []string{"a/b/c"}, stderr: "'a/b/c' must be OWNER/NAME"},
		"web-url without http":   {args: []string{"--web-url", "web.test", "acme/gastown"}, stderr: "the web root (--web-url or FORGEJO_URL) must be http:// or https://"},
		"no HOME, no backup dir": {args: []string{"acme/gastown"}, stderr: "--backup-dir is required"},
	}
	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run(tc.args, noEnv, &stdout, &stderr); got != forgejoresync.ExitUsage {
				t.Fatalf("run(%v) = %d, want 2 (stderr: %s)", tc.args, got, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("run(%v) stderr = %q, want it to hold %q", tc.args, stderr.String(), tc.stderr)
			}
			if !strings.Contains(stderr.String(), "usage: forgejo-resync --help") {
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
			if got := run([]string{flag}, noEnv, &stdout, &stderr); got != forgejoresync.ExitOK {
				t.Fatalf("run(%s) = %d, want 0 (stderr: %s)", flag, got, stderr.String())
			}
			// Each of these is read from the constant that holds the default, so a
			// default the text no longer names fails here.
			for _, want := range []string{
				"Usage: forgejo-resync [options] OWNER/NAME",
				"--dry-run",
				"else " + forgejoresync.DefaultAPIURL + ")",
				"(default: " + forgejoresync.DefaultMainBranch + ")",
				"$HOME/forgejo/backups)",
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
}

// TestMainValidCommandLineWithoutTokenExitsOne: a command line that parses and
// reaches the run, with no admin token, is a failed run: exit 1, the broker
// command named, and no usage hint, since the command line itself was valid.
func TestMainValidCommandLineWithoutTokenExitsOne(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	got := run([]string{"--dry-run", "acme/gastown"}, homeEnv, &stdout, &stderr)
	if got != forgejoresync.ExitFailure {
		t.Fatalf("run = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no admin token") {
		t.Errorf("stderr = %q, want the missing token named", stderr.String())
	}
	if strings.Contains(stderr.String(), "usage: forgejo-resync --help") {
		t.Errorf("stderr = %q, want no usage hint: the command line was valid", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing from a failed run", stdout.String())
	}
}

// TestRunExitCodes pins the exit codes the command reports: help is 0, a refused
// command line is 2, and a run that cannot start for want of an admin token is 1.
// The token case stops before any git or HTTP call, so it is hermetic.
func TestRunExitCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		args   []string
		getenv func(string) string
		want   int
	}{
		{name: "help", args: []string{"--help"}, getenv: noEnv, want: forgejoresync.ExitOK},
		{name: "bad flag", args: []string{"--nope", "acme/gastown"}, getenv: homeEnv, want: forgejoresync.ExitUsage},
		{name: "no admin token", args: []string{"--dry-run", "acme/gastown"}, getenv: homeEnv, want: forgejoresync.ExitFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run(tc.args, tc.getenv, &stdout, &stderr); got != tc.want {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", tc.args, got, tc.want, stderr.String())
			}
		})
	}
}

// TestMainParseWithoutOptionsExitsOne: a parse seam that returns neither options
// nor an error has no command line to run. It exits 1 and says so; reading the
// missing options would panic instead of reporting the failed run.
func TestMainParseWithoutOptionsExitsOne(t *testing.T) {
	t.Parallel()
	empty := func([]string, func(string) string) (*forgejoresync.Options, error) {
		return nil, nil
	}
	var stdout, stderr strings.Builder
	if got := runParse(nil, noEnv, &stdout, &stderr, empty); got != forgejoresync.ExitFailure {
		t.Fatalf("runParse = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "the command line parsed to no options") {
		t.Errorf("stderr = %q, want the missing options named", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing on a failed run", stdout.String())
	}
}

// TestMainParseFailureExitsOne: a parse failure that is not a usage error is a
// failed run (1) with its own line, not a refused command line (2) with the
// usage hint. Parse reports no such failure today; the seam stands so a later
// bead reading the admin token file at parse time cannot turn its failure into
// a usage refusal.
func TestMainParseFailureExitsOne(t *testing.T) {
	t.Parallel()
	unreadable := func([]string, func(string) string) (*forgejoresync.Options, error) {
		return nil, errors.New("the admin token file is unreadable")
	}
	var stdout, stderr strings.Builder
	if got := runParse(nil, noEnv, &stdout, &stderr, unreadable); got != forgejoresync.ExitFailure {
		t.Fatalf("runParse = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "the admin token file is unreadable") {
		t.Errorf("stderr = %q, want the parse failure named", stderr.String())
	}
	if strings.Contains(stderr.String(), "usage: forgejo-resync --help") {
		t.Errorf("stderr = %q, want no usage hint: this is not a refused command line", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing on a failed run", stdout.String())
	}
}

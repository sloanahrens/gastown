package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// noEnv is the empty environment: every default these tests read is one no
// variable moved.
func noEnv(string) string { return "" }

// emptyEnv is the Env of a command line that is refused or only asks for help:
// the environment is empty, and no run reaches the seams beyond it.
func emptyEnv() forgejoprov.Env { return forgejoprov.Env{Getenv: noEnv} }

// TestMainUsageExitsTwo holds the script's usage_die: the message names the
// flag, the usage hint follows it, and the code is 2.
func TestMainUsageExitsTwo(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args   []string
		stderr string
	}{
		"unknown option":            {args: []string{"--nope"}, stderr: "unknown option: --nope"},
		"unknown role":              {args: []string{"--role", "admin"}, stderr: "unknown role 'admin'"},
		"role with a slash":         {args: []string{"--role", "bad/role"}, stderr: "--role 'bad/role' must match"},
		"--repo without OWNER/NAME": {args: []string{"--repo", "gastown"}, stderr: "--repo 'gastown' must be OWNER/NAME"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run(tc.args, emptyEnv(), &stdout, &stderr); got != 2 {
				t.Fatalf("run(%v) = %d, want 2 (stderr: %s)", tc.args, got, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("run(%v) stderr = %q, want it to hold %q", tc.args, stderr.String(), tc.stderr)
			}
			if !strings.Contains(stderr.String(), "usage: forgejo-provision --help") {
				t.Errorf("run(%v) stderr = %q, want the usage hint", tc.args, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("run(%v) stdout = %q, want nothing on a refusal", tc.args, stdout.String())
			}
		})
	}
}

// TestMainHelpExitsZero holds both spellings of the help request: the flag list
// on stdout and success, so `forgejo-provision -h | grep` works.
func TestMainHelpExitsZero(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run([]string{flag}, emptyEnv(), &stdout, &stderr); got != 0 {
				t.Fatalf("run(%s) = %d, want 0 (stderr: %s)", flag, got, stderr.String())
			}
			// Each of these is read from the constant that holds the default, so
			// a default the text no longer names fails here.
			for _, want := range []string{
				"Usage: forgejo-provision",
				"--api-url",
				"--dry-run",
				"else " + forgejoprov.DefaultAPIURL,
				"(default: " + strings.Join(forgejoprov.DefaultRoles(), " ") + ";",
				"known: " + forgejoprov.KnownRoles() + ")",
				"(default: " + forgejoprov.DefaultBotPrefix + ")",
				"(default: " + forgejoprov.DefaultBotEmailDomain + ")",
				"(default: " + forgejoprov.DefaultTokenName + ")",
				`(default: "ci / gate (push)" and "om / review")`,
				"(default: " + forgejoprov.DefaultMainBranch + ")",
				"(default: " + forgejoprov.DefaultLandBranch + ")",
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

// TestMainParseFailureExitsOne: a parse failure that is not a usage error is a
// failed run (1) with its own line, not a refused command line (2) with the
// usage hint. Parse reports no such failure today; the seam stands so a later
// bead reading the admin token file at parse time cannot turn its failure into
// a usage refusal.
func TestMainParseFailureExitsOne(t *testing.T) {
	t.Parallel()
	unreadable := func([]string, func(string) string) (*forgejoprov.Options, error) {
		return nil, errors.New("the admin token file is unreadable")
	}
	var stdout, stderr strings.Builder
	if got := runParse(nil, emptyEnv(), &stdout, &stderr, unreadable); got != forgejoprov.ExitFailure {
		t.Fatalf("runParse = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "the admin token file is unreadable") {
		t.Errorf("stderr = %q, want the parse failure named", stderr.String())
	}
	if strings.Contains(stderr.String(), "usage: forgejo-provision --help") {
		t.Errorf("stderr = %q, want no usage hint: this is not a refused command line", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing on a failed run", stdout.String())
	}
}

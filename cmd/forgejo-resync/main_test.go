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

// TestMainValidCommandLineFailsUntilRunIsPorted: a command line that parses has
// no run behind it in this build, so it exits 1 and says so. Exiting 0 here
// would report a resync that never happened.
func TestMainValidCommandLineFailsUntilRunIsPorted(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	got := run([]string{"--dry-run", "acme/gastown"}, noEnv, &stdout, &stderr)
	if got != forgejoresync.ExitFailure {
		t.Fatalf("run = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "the run for acme/gastown is not ported yet") {
		t.Errorf("stderr = %q, want the unported run named", stderr.String())
	}
	if strings.Contains(stderr.String(), "usage: forgejo-resync --help") {
		t.Errorf("stderr = %q, want no usage hint: the command line was valid", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing from an unported run", stdout.String())
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

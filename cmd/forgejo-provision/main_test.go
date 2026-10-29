package main

import (
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// noEnv is the empty environment: every default these tests read is one no
// variable moved.
func noEnv(string) string { return "" }

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
		"--repo without OWNER/NAME": {args: []string{"--repo", "gastown"}, stderr: "--repo 'gastown' must be OWNER/NAME"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run(tc.args, noEnv, &stdout, &stderr); got != 2 {
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
			if got := run([]string{flag}, noEnv, &stdout, &stderr); got != 0 {
				t.Fatalf("run(%s) = %d, want 0 (stderr: %s)", flag, got, stderr.String())
			}
			for _, want := range []string{"Usage: forgejo-provision", "--api-url", "--dry-run"} {
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

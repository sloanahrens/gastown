package main

import (
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// Until a section lands, every run ends in the guard: one result line, status
// 1, and for a bad argument the script's message on stderr first.
func TestRunEndsWithTheContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		args       []string
		wantLast   string
		wantStderr string
	}{
		{
			name:     "no arguments names origin/main, the SHA it started from",
			wantLast: "install-gt: RESULT failed origin/main - unexpected",
		},
		{
			name:     "the SHA argument is named before it is resolved",
			args:     []string{"--sha", "abc123"},
			wantLast: "install-gt: RESULT failed abc123 - unexpected",
		},
		{
			name:       "an empty --sha names nothing",
			args:       []string{"--sha", ""},
			wantLast:   "install-gt: RESULT failed - - unexpected",
			wantStderr: "install-gt: --sha needs a commit",
		},
		{
			name:       "a bad source still ends in a result line",
			args:       []string{"--sha", "abc123", "--source", "bogus"},
			wantLast:   "install-gt: RESULT failed abc123 - unexpected",
			wantStderr: "install-gt: --source must be manual, rebuild-gt or post-land",
		},
		{
			name:       "an unknown argument stops parsing where it stands",
			args:       []string{"--bogus", "--sha", "abc123"},
			wantLast:   "install-gt: RESULT failed origin/main - unexpected",
			wantStderr: "install-gt: unknown argument: --bogus",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut strings.Builder

			code := run(tc.args, &out, &errOut)

			if code != 1 {
				t.Errorf("run(%q) = %d, want 1", tc.args, code)
			}
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if len(lines) != 1 {
				t.Fatalf("stdout = %q, want one line", out.String())
			}
			if lines[0] != tc.wantLast {
				t.Errorf("last line = %q, want %q", lines[0], tc.wantLast)
			}
			if got := strings.TrimSuffix(errOut.String(), "\n"); got != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tc.wantStderr)
			}
		})
	}
}

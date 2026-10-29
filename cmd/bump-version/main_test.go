package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bumpversion"
	"github.com/steveyegge/gastown/internal/testutil"
)

// The command runs in this test process; the hermetic harness keeps the live
// town's HOME and GT_* environment out of reach of the git steps the later
// slices add.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// runCapture runs the command and returns its exit code and everything it
// wrote to the error stream.
func runCapture(args []string) (int, string) {
	var errw bytes.Buffer
	code := run(args, &errw)
	return code, errw.String()
}

// TestMainUsageAndUnknownOption: an empty command line prints Usage alone, and
// an unknown option names itself before the same Usage, both exiting 1.
func TestMainUsageAndUnknownOption(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		wantHead string // first line of the error stream; Usage for the empty line
	}{
		{name: "no arguments", args: nil, wantHead: "Usage: bump-version <version> [--commit] [--tag] [--install] [--allow-dirty]"},
		{name: "unknown option", args: []string{"0.2.0", "--bogus"}, wantHead: "Unknown option '--bogus'"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, got := runCapture(tc.args)
			if code != 1 {
				t.Errorf("run(%v) exit = %d, want 1", tc.args, code)
			}
			if head := strings.SplitN(got, "\n", 2)[0]; head != tc.wantHead {
				t.Errorf("run(%v) first line = %q, want %q", tc.args, head, tc.wantHead)
			}
			if !strings.HasSuffix(got, bumpversion.Usage) {
				t.Errorf("run(%v) did not end with Usage, got:\n%s", tc.args, got)
			}
		})
	}
}

// TestMainRefusals: a bad version and a bad flag combination each print their
// own explanation, with no Usage, and exit 1.
func TestMainRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string // the whole error stream
	}{
		{
			name: "invalid version",
			args: []string{"1.0"},
			want: "Invalid version format '1.0'\n" +
				"Expected semantic version format: MAJOR.MINOR.PATCH (e.g., 0.2.0)\n",
		},
		{
			name: "tag without commit",
			args: []string{"0.2.0", "--tag"},
			want: "--tag requires --commit\n",
		},
		{
			name: "push refused",
			args: []string{"0.2.0", "--push"},
			want: "--push is not offered: this command never runs git push, because main lands only through the landing worker and the tag is pushed by the release procedure\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, got := runCapture(tc.args)
			if code != 1 {
				t.Errorf("run(%v) exit = %d, want 1", tc.args, code)
			}
			if got != tc.want {
				t.Errorf("run(%v) wrote %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestMainAcceptableCommandLine: a readable command line exits 0 and prints
// nothing, since the run arrives in a later slice.
func TestMainAcceptableCommandLine(t *testing.T) {
	t.Parallel()
	code, got := runCapture([]string{"0.2.0", "--commit", "--tag", "--install"})
	if code != 0 {
		t.Errorf("run with a valid command line exit = %d, want 0", code)
	}
	if got != "" {
		t.Errorf("run with a valid command line wrote %q, want nothing", got)
	}
}

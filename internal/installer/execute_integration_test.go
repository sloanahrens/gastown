//go:build integration

package installer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIntegrationRealExecuteReportsTheStatusBashWould pins the two numbers a
// command that never ran is reported with, so the slot step's 126/127 reading
// holds: a path that is there and not executable is 126, and a name nothing
// can execute is 127. It is an integration test because RealExecute asks the
// kernel to exec, which a unit test must not do (gt-tpg0p.11).
func TestIntegrationRealExecuteReportsTheStatusBashWould(t *testing.T) {
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "gt")
	if err := os.WriteFile(notExecutable, []byte("not a program\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		// command is the name to run.
		command string
		want    int
	}{
		{
			name:    "a path that exists and cannot be executed",
			command: notExecutable,
			want:    126,
		},
		{
			name:    "a path that is not there",
			command: filepath.Join(dir, "missing"),
			want:    127,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := RealExecute("", tc.command, nil)

			if code != tc.want {
				t.Errorf("RealExecute(%q) = %d, want %d (stdout %q, stderr %q)", tc.command, code, tc.want, stdout, stderr)
			}
		})
	}
}

// TestIntegrationRealExecuteFeedsStdinToTheCommand is the wiring the keychain
// password goes through in an unattended install: the seam a unit test scripts
// only carries the bytes to RealExecute's caller, so only a real process shows
// that they reach the command's standard input. A nil stdin must leave it at
// /dev/null, which the unlock's line never is but every other command's is
// (gt-4ucnp).
func TestIntegrationRealExecuteFeedsStdinToTheCommand(t *testing.T) {
	cases := []struct {
		name  string
		stdin []byte
		want  string
	}{
		{
			name:  "the line reaches the command",
			stdin: []byte("unlock-keychain -p secret /k\n"),
			want:  "unlock-keychain -p secret /k\n",
		},
		{
			name:  "no stdin leaves the command at /dev/null",
			stdin: nil,
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := RealExecute("", "cat", tc.stdin)

			if code != 0 {
				t.Fatalf("RealExecute(cat) = %d, want 0 (stdout %q, stderr %q)", code, stdout, stderr)
			}
			if got := string(stdout); got != tc.want {
				t.Errorf("command read %q on stdin, want %q", got, tc.want)
			}
		})
	}
}

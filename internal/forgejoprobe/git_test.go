package forgejoprobe

import (
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestGitRunScrubsTheChildEnvironment drives Run through its process seam and
// reads the environment git is started with: the admin token variables, a
// caller-supplied token and the repository pins are absent, and HOME stays.
// Removing the scrub from Run turns it red, since GitChildEnv is the only filter.
func TestGitRunScrubsTheChildEnvironment(t *testing.T) {
	t.Parallel()
	var gotEnv []string
	g := execGit{
		run: func(cmd *exec.Cmd) error {
			gotEnv = cmd.Env
			return nil
		},
		environ: func() []string {
			return []string{
				"HOME=/home/user",
				"GT_AUTH_FORGEJO_TOKEN=broker-token",
				"FORGEJO_ADMIN_TOKEN=admin-token",
				"GIT_DIR=/elsewhere/.git",
			}
		},
	}
	if _, err := g.Run("work/copy", []string{"FOO_TOKEN=caller-token"}, "status"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, entry := range gotEnv {
		for _, token := range []string{"broker-token", "admin-token", "caller-token"} {
			if strings.Contains(entry, token) {
				t.Errorf("git was started with a token in its environment (%s)", strings.SplitN(entry, "=", 2)[0])
			}
		}
	}
	for _, name := range []string{"GIT_DIR", "GT_AUTH_FORGEJO_TOKEN", "FORGEJO_ADMIN_TOKEN", "FOO_TOKEN"} {
		if slices.ContainsFunc(gotEnv, func(kv string) bool { return strings.HasPrefix(kv, name+"=") }) {
			t.Errorf("git was started with %s in its environment", name)
		}
	}
	if !slices.Contains(gotEnv, "HOME=/home/user") {
		t.Errorf("git lost HOME, which the credential helpers need")
	}
}

// TestGitRunReturnsGitsOutput drives Run's three outcomes through the process
// seam: a success returns stdout; a failure carries git's stderr on the error,
// or the bare exit error when git said nothing. Each sees the argv and the
// directory git was run in.
func TestGitRunReturnsGitsOutput(t *testing.T) {
	t.Parallel()
	exitErr := errors.New("exit status 128")
	cases := []struct {
		name    string
		stdout  string
		stderr  string
		fail    bool
		wantOut string
		wantErr string
	}{
		{name: "a success returns stdout", stdout: "0123abcd\n", wantOut: "0123abcd\n"},
		{name: "a failure carries git's stderr", stderr: "fatal: bad revision\n", fail: true, wantErr: "exit status 128: fatal: bad revision"},
		{name: "a failure with no stderr is the bare exit error", fail: true, wantErr: "exit status 128"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotDir string
			var gotArgs []string
			g := execGit{run: func(cmd *exec.Cmd) error {
				gotDir, gotArgs = cmd.Dir, cmd.Args[1:]
				_, _ = io.WriteString(cmd.Stdout, tc.stdout)
				_, _ = io.WriteString(cmd.Stderr, tc.stderr)
				if tc.fail {
					return exitErr
				}
				return nil
			}}
			out, err := g.Run("work/copy", nil, "rev-parse", "--verify", "HEAD")
			if out != tc.wantOut {
				t.Errorf("stdout = %q, want %q", out, tc.wantOut)
			}
			if gotDir != "work/copy" || !slices.Equal(gotArgs, []string{"rev-parse", "--verify", "HEAD"}) {
				t.Errorf("git ran in %q with args %q, want work/copy with rev-parse --verify HEAD", gotDir, gotArgs)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Run: %v, want success", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if !errors.Is(err, exitErr) {
				t.Errorf("error %v does not wrap the exit error", err)
			}
		})
	}
}

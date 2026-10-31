package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/steveyegge/gastown/internal/forgejo"
)

const credTestToken = "tok-s3cret-value"

// credTokenFile writes a token file with mode and returns its path.
func credTokenFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forgejo-landing.env")
	body := "FORGEJO_USER=bot-landing\nFORGEJO_TOKEN=" + credTestToken + "\n"
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod token file: %v", err)
	}
	return path
}

// runCredentialCmd drives the real command: its flags parse from args and
// stdin is request. It returns stdout, stderr (the error text) and the error.
func runCredentialCmd(t *testing.T, request string, args ...string) (stdout, errText string, err error) {
	t.Helper()
	var out bytes.Buffer
	c := newGitCredentialForgejoCmd(strings.NewReader(request), &out, forgejo.ReadCredentialFile)
	c.SetArgs(args)
	var errBuf bytes.Buffer
	c.SetErr(&errBuf)
	c.SetOut(&errBuf)
	err = c.Execute()
	if err != nil {
		errText = err.Error()
	}
	return out.String(), errText + errBuf.String(), err
}

const credRequest = "protocol=http\nhost=scratch-forgejo:3000\npath=scratch-admin/l2rig.git\n\n"

func TestGitCredentialForgejoGet(t *testing.T) {
	t.Parallel()
	good := credTokenFile(t, 0o600)

	t.Run("an allowlisted host gets the login and the token", func(t *testing.T) {
		t.Parallel()
		out, _, err := runCredentialCmd(t, credRequest, "--host", "scratch-forgejo:3000", "--token-file", good, "get")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		want := "username=bot-landing\npassword=" + credTestToken + "\n"
		if out != want {
			t.Errorf("stdout = %q, want %q", out, want)
		}
	})

	t.Run("a host written in another case matches", func(t *testing.T) {
		t.Parallel()
		out, _, err := runCredentialCmd(t, "protocol=https\nhost=Scratch-Forgejo:3000\n\n", "--host", "scratch-forgejo:3000", "--token-file", good, "get")
		if err != nil || !strings.Contains(out, "password=") {
			t.Errorf("get = %q, %v; want an answer", out, err)
		}
	})

	for _, tc := range []struct{ name, request string }{
		{"a different host", "protocol=http\nhost=other-forgejo:3000\n\n"},
		{"the same host on a different port", "protocol=http\nhost=scratch-forgejo:3001\n\n"},
		{"the same host with no port", "protocol=http\nhost=scratch-forgejo\n\n"},
		{"a protocol that is not http", "protocol=ssh\nhost=scratch-forgejo:3000\n\n"},
		{"no protocol", "host=scratch-forgejo:3000\n\n"},
		{"no host", "protocol=http\n\n"},
	} {
		t.Run(tc.name+" gets no answer", func(t *testing.T) {
			t.Parallel()
			out, errText, err := runCredentialCmd(t, tc.request, "--host", "scratch-forgejo:3000", "--token-file", good, "get")
			if err != nil || out != "" || errText != "" {
				t.Errorf("get = %q, error %v, stderr %q; want empty output and exit 0", out, err, errText)
			}
		})
	}

	// A token file git may not trust is refused, and the token stays off both
	// streams: the loose file is read for its mode before its content.
	t.Run("a loose-mode token file is refused without the token on any stream", func(t *testing.T) {
		t.Parallel()
		loose := credTokenFile(t, 0o644)
		out, errText, err := runCredentialCmd(t, credRequest, "--host", "scratch-forgejo:3000", "--token-file", loose, "get")
		if err == nil {
			t.Fatal("get on a mode 644 token file succeeded, want a refusal")
		}
		if strings.Contains(out, credTestToken) || strings.Contains(errText, credTestToken) {
			t.Errorf("the token reached a stream: stdout %q, stderr %q", out, errText)
		}
		if out != "" {
			t.Errorf("stdout = %q, want empty", out)
		}
	})

	t.Run("a missing token file is refused", func(t *testing.T) {
		t.Parallel()
		_, _, err := runCredentialCmd(t, credRequest, "--host", "scratch-forgejo:3000", "--token-file", filepath.Join(t.TempDir(), "absent.env"), "get")
		if err == nil {
			t.Error("get on a missing token file succeeded, want a refusal")
		}
	})
}

// The flags are checked before the request is read, so a mis-set helper fails
// loudly instead of answering for the wrong host.
func TestGitCredentialForgejoRefusesFlagsItCannotUse(t *testing.T) {
	t.Parallel()
	abs := credTokenFile(t, 0o600)
	for _, tc := range []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{"no host", []string{"--token-file", abs, "get"}, "--host is required"},
		{"an empty host", []string{"--host", " ", "--token-file", abs, "get"}, "is empty"},
		{"a host beginning with a dash", []string{"--host", "-h", "--token-file", abs, "get"}, "begins with a dash"},
		{"a host with a scheme", []string{"--host", "http://scratch-forgejo:3000", "--token-file", abs, "get"}, "bare host:port"},
		{"a host with a login", []string{"--host", "bot@scratch-forgejo:3000", "--token-file", abs, "get"}, "bare host:port"},
		{"no token file", []string{"--host", "scratch-forgejo:3000", "get"}, "absolute path"},
		{"a relative token file", []string{"--host", "scratch-forgejo:3000", "--token-file", "forgejo-landing.env", "get"}, "absolute path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, errText, err := runCredentialCmd(t, credRequest, tc.args...)
			if err == nil || !strings.Contains(errText, tc.wantMsg) {
				t.Fatalf("error = %v, want a refusal saying %q", err, tc.wantMsg)
			}
			if out != "" {
				t.Errorf("stdout = %q, want empty", out)
			}
		})
	}
}

// store and erase do nothing: they read no request and no token file, so a
// missing file is not an error for them, and an operation git does not define
// is ignored the same way.
func TestGitCredentialForgejoStoreAndErase(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent.env")
	for _, op := range []string{"store", "erase", "capability"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			out, errText, err := runCredentialCmd(t, credRequest, "--host", "scratch-forgejo:3000", "--token-file", missing, op)
			if err != nil || out != "" || errText != "" {
				t.Errorf("%s = %q, error %v, stderr %q; want nothing", op, out, err, errText)
			}
		})
	}
}

// A request that cannot be read, and an answer that cannot be written, are
// errors rather than a silent empty answer.
func TestGitCredentialForgejoReportsIOFailures(t *testing.T) {
	t.Parallel()
	flags := gitCredentialForgejoFlags{hosts: []string{"scratch-forgejo:3000"}, tokenFile: credTokenFile(t, 0o600)}

	err := runGitCredentialForgejo("get", flags, iotest.ErrReader(errors.New("pipe closed")), &bytes.Buffer{}, forgejo.ReadCredentialFile)
	if err == nil || !strings.Contains(err.Error(), "read the credential request") {
		t.Errorf("unreadable request: error = %v, want one naming the request", err)
	}

	err = runGitCredentialForgejo("get", flags, strings.NewReader(credRequest), credFailingWriter{}, forgejo.ReadCredentialFile)
	if err == nil {
		t.Error("unwritable answer: error = nil, want the write error")
	}
}

type credFailingWriter struct{}

func (credFailingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

// The command is registered, hidden, and exempt from the checks that would
// write to stderr or the usage log on every git push.
func TestGitCredentialForgejoIsRegisteredHiddenAndExempt(t *testing.T) {
	t.Parallel()
	if gitCredentialForgejoCmd.Parent() != rootCmd {
		t.Fatal("git-credential-forgejo is not a command of root")
	}
	if !gitCredentialForgejoCmd.Hidden {
		t.Error("git-credential-forgejo is not hidden")
	}
	for name, m := range map[string]map[string]bool{
		"beadsExemptCommands":       beadsExemptCommands,
		"branchCheckExemptCommands": branchCheckExemptCommands,
		"noLogCommands":             noLogCommands,
	} {
		if !m["git-credential-forgejo"] {
			t.Errorf("git-credential-forgejo is missing from %s", name)
		}
	}
}

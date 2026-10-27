//go:build integration && darwin

package installer

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// securityDeadline bounds each real security(1) call. A keychain that is still
// locked prompts on the terminal, which an unattended install never answers;
// the deadline turns that prompt into a failure instead of a hung test.
const securityDeadline = 30 * time.Second

// TestIntegrationSecurityReadsTheUnlockLineFromStdin is the macOS half of
// gt-4ucnp: Sign hands security the keychain password on stdin, a form
// security(1) does not document, so this runs the real security with the exact
// line Sign builds, against a throwaway keychain whose password needs quoting.
// It pins the three things Sign assumes: the line parses, the unlock takes (an
// item added before the lock reads back after it), and security -i exits 0 on
// a successful unlock, which is the status Sign reads.
//
// The keychain lives in a temporary directory and is deleted afterwards;
// create-keychain puts it in the search list, and delete-keychain takes it out
// again.
func TestIntegrationSecurityReadsTheUnlockLineFromStdin(t *testing.T) {
	if _, err := exec.LookPath("security"); err != nil {
		t.Fatalf("security is not on PATH: %v", err)
	}
	cases := []struct {
		name     string
		password string
	}{
		{name: "an ordinary password", password: "s3cret"},
		{name: "a password with a space", password: "two words"},
		{name: "a password with a double quote", password: `pa"ss`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keychain := filepath.Join(t.TempDir(), "gt-unlock-test.keychain-db")
			securityRun(t, "create-keychain", tc.password, "", "create-keychain", "-p", tc.password, keychain)
			t.Cleanup(func() {
				// Never fatal: a keychain left in the search list outlives the
				// test, and the next run should still be able to start.
				ctx, cancel := context.WithTimeout(context.Background(), securityDeadline)
				defer cancel()
				if err := exec.CommandContext(ctx, "security", "delete-keychain", keychain).Run(); err != nil {
					t.Logf("delete-keychain %s: %v; remove it by hand if it stays in the search list", keychain, err)
				}
			})
			securityRun(t, "unlock-keychain", tc.password, "", "unlock-keychain", "-p", tc.password, keychain)
			securityRun(t, "add-generic-password", tc.password, "",
				"add-generic-password", "-a", "gt-4ucnp", "-s", "gt-4ucnp", "-w", "probe-value", keychain)
			securityRun(t, "lock-keychain", tc.password, "", "lock-keychain", keychain)

			line, ok := unlockLine(tc.password, keychain)
			if !ok {
				t.Fatalf("unlockLine refused a one-line password")
			}
			stdout, stderr, code := securityRun(t, "security -i", tc.password, line, "-i")
			if code != 0 {
				t.Fatalf("security -i exited %d on the unlock line (stdout %q, stderr %q); Sign reads 0 as the unlock's success and skips codesign, so sign.go must be changed to stop reading this status", code, stdout, stderr)
			}
			// The item was unreadable while the keychain was locked, so reading
			// it back is the unlock taking effect.
			stdout, stderr, code = securityRun(t, "find-generic-password", tc.password, "",
				"find-generic-password", "-a", "gt-4ucnp", "-s", "gt-4ucnp", "-w", keychain)
			if code != 0 {
				t.Fatalf("find-generic-password exited %d after the unlock (stdout %q, stderr %q); the keychain stayed locked, so the line security -i read did not unlock it", code, stdout, stderr)
			}
			if got := strings.TrimSpace(stdout); got != "probe-value" {
				t.Errorf("read back %q, want the probe value the locked keychain held", got)
			}
		})
	}
}

// securityRun runs the real security with args, feeding stdin when it is not
// empty, and bounds the call by securityDeadline. secret is the password the
// call must not print. Only label names the call in a failure: the args of a
// call that carries the password must not reach an error message.
func securityRun(t *testing.T, label, secret, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), securityDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s did not finish within %s; a locked keychain prompts on the terminal", label, securityDeadline)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("%s: %v", label, err)
	}
	if secret != "" && strings.Contains(out.String()+errOut.String(), secret) {
		t.Errorf("%s printed the keychain password", label)
	}
	return out.String(), errOut.String(), code
}

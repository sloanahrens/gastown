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
// a successful unlock, which Sign reads.
//
// The keychain lives in a temporary directory and is deleted afterwards. The
// user keychain search list, which create-keychain changes, is put back too.
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
			before := userSearchList(t)
			keychain := filepath.Join(t.TempDir(), "gt-unlock-test.keychain-db")
			t.Cleanup(func() { removeKeychainAndRestore(t, keychain, before) })

			// The setup never puts the keychain password in argv either: every
			// line that carries it goes through security -i on stdin, the same
			// way Sign unlocks.
			requireSecurityLines(t,
				"create-keychain -p "+lineWord(tc.password)+" "+lineWord(keychain),
				"add-generic-password -a gt-4ucnp -s gt-4ucnp -w probe-value "+lineWord(keychain),
				"lock-keychain "+lineWord(keychain),
			)

			line, ok := unlockLine(tc.password, keychain)
			if !ok {
				t.Fatalf("unlockLine refused a one-line password")
			}
			if _, _, code := securityRun(t, "security -i", tc.password, line, "-i"); code != 0 {
				// The output can hold the password, so it never reaches this
				// message: securityRun reports a leak separately. Sign reads 0
				// as the unlock's success, so a non-zero here means Sign must
				// stop reading this status.
				t.Fatalf("security -i exited %d on the unlock line", code)
			}
			// The item was unreadable while the keychain was locked, so reading
			// it back is the unlock taking effect.
			stdout, _, code := securityRun(t, "find-generic-password", "", "",
				"find-generic-password", "-a", "gt-4ucnp", "-s", "gt-4ucnp", "-w", keychain)
			if code != 0 {
				t.Fatalf("find-generic-password exited %d after the unlock; the keychain stayed locked, so the line security -i read did not unlock it", code)
			}
			if got := strings.TrimSpace(stdout); got != "probe-value" {
				t.Errorf("read back %q, want the probe value the locked keychain held", got)
			}
		})
	}
}

// requireSecurityLines runs each line through security -i, which is how the
// test creates and locks its keychain without putting the password in argv.
// A line that carries the password is not passed as the secret to check: the
// output of a setup command is not what this test is about, and the unlock
// call below is the one whose output must stay clean.
func requireSecurityLines(t *testing.T, lines ...string) {
	t.Helper()
	for _, line := range lines {
		label := strings.Fields(line)[0] + " via security -i"
		if _, _, code := securityRun(t, label, "", line+"\n", "-i"); code != 0 {
			t.Fatalf("%s exited %d; security -i did not accept a setup line", label, code)
		}
	}
}

// lineWord quotes one word of a security -i line. It is written here rather
// than taken from the production quoteLine so that broken production quoting
// is caught by the unlock assertions instead of being reused to build the
// setup the test starts from.
func lineWord(word string) string {
	if word != "" && !strings.ContainsAny(word, " \t\"'\\") {
		return word
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(word) + `"`
}

// userSearchList is the developer's user keychain search list, which
// create-keychain changes; the test puts it back so a run leaves no trace.
func userSearchList(t *testing.T) []string {
	t.Helper()
	out, _, code := securityRun(t, "list-keychains", "", "", "list-keychains", "-d", "user")
	if code != 0 {
		t.Fatalf("list-keychains exited %d", code)
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.Trim(strings.TrimSpace(line), `"`); line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

// removeKeychainAndRestore deletes the throwaway keychain and sets the user
// search list back to what it was. Both are best effort: a cleanup failure is
// logged, never fatal, so it cannot mask the test's own result.
func removeKeychainAndRestore(t *testing.T, keychain string, before []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), securityDeadline)
	defer cancel()
	if err := exec.CommandContext(ctx, "security", "delete-keychain", keychain).Run(); err != nil {
		t.Logf("delete-keychain %s: %v; remove it by hand if it stays in the search list", keychain, err)
	}
	if len(before) > 0 {
		args := append([]string{"list-keychains", "-d", "user", "-s"}, before...)
		if err := exec.CommandContext(ctx, "security", args...).Run(); err != nil {
			t.Logf("restore the user keychain search list: %v", err)
		}
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

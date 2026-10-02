package git

import (
	"errors"
	"strings"
	"testing"
)

// A local pre-push hook refuses a push by exiting non-zero and printing its
// reason on stdout, while git's own summary goes to stderr. Both have to
// reach the caller: the red-main revert push failed (gt-pihe2) and the log
// held nothing but "error: failed to push some refs to ...", which named no
// rule and no branch family.
func TestPushForceWithLeaseErrorCarriesHookStdout(t *testing.T) {
	t.Parallel()
	const (
		branch  = "revert/gt-4k3fj.8.6-69e3bc54"
		refspec = "refs/heads/" + branch + ":refs/heads/" + branch
		reason  = "ERROR: Invalid branch for Gas Town agents."
		summary = "error: failed to push some refs to 'https://github.com/sloanahrens/gastown.git'\n"
	)
	s := plainRemotes().on(
		"push origin "+refspec+" --force-with-lease=refs/heads/"+branch+":",
		reply{stdout: reason + "\n\nBlocked push to: " + branch + "\n", stderr: summary, code: 1},
	)
	err := newTestGit(t, s).PushForceWithLease("origin", refspec, "refs/heads/"+branch, "")
	if err == nil {
		t.Fatal("a refused push must fail")
	}
	if !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "Blocked push to: "+branch) {
		t.Errorf("error %q does not carry the hook's reason", err)
	}
	if !strings.Contains(err.Error(), "failed to push some refs") {
		t.Errorf("error %q does not carry git's own summary", err)
	}
	var ge *GitError
	if !errors.As(err, &ge) {
		t.Fatalf("error %v no longer unwraps to *GitError", err)
	}
	s.noUnscripted(t)
}

// Push is the landing worker's revert path (HEAD:refs/heads/<branch>), and it
// takes the same hook refusal.
func TestPushErrorCarriesHookStdout(t *testing.T) {
	t.Parallel()
	const (
		branch = "revert/gt-4k3fj.8.6-69e3bc54"
		reason = "ERROR: Invalid branch for Gas Town agents."
	)
	s := plainRemotes().on(
		"push origin HEAD:refs/heads/"+branch,
		reply{stdout: reason + "\n", stderr: "error: failed to push some refs to 'origin'\n", code: 1},
	)
	err := newTestGit(t, s).Push("origin", "HEAD:refs/heads/"+branch, false)
	if err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("error = %v, want it to carry the hook's reason", err)
	}
	s.noUnscripted(t)
}

// A push that failed for a reason git itself explains is unchanged: git wrote
// nothing to stdout, so there is nothing to add.
func TestPushErrorWithoutStdoutIsUnchanged(t *testing.T) {
	t.Parallel()
	s := plainRemotes().on("push origin main",
		fail(1, " ! [rejected]        main -> main (fetch first)\nerror: failed to push some refs to 'origin'\n"))
	err := newTestGit(t, s).Push("origin", "main", false)
	var ge *GitError
	if err == nil || !errors.As(err, &ge) {
		t.Fatalf("error = %v, want a *GitError", err)
	}
	if strings.Contains(err.Error(), "\n\n") || !strings.HasPrefix(err.Error(), "git push: ") {
		t.Fatalf("error = %q, want git's stderr alone", err)
	}
	if !strings.Contains(err.Error(), "[rejected]") {
		t.Fatalf("error = %q, want git's hint lines", err)
	}
}

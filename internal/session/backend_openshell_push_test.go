package session

import (
	"errors"
	"testing"
	"time"
)

// fakePushGit scripts the git calls of the push step and records whether a
// force was asked for.
type fakePushGit struct {
	pushErr    error
	fetchErr   error
	revs       map[string]string // ref -> sha; a missing ref is an error
	ancestor   bool
	ancestorEr error
	forced     bool
}

func (f *fakePushGit) Push(_, _ string, force bool) error {
	f.forced = f.forced || force
	return f.pushErr
}
func (f *fakePushGit) FetchBranchWithTimeout(string, string, time.Duration) error { return f.fetchErr }
func (f *fakePushGit) Rev(ref string) (string, error) {
	sha, ok := f.revs[ref]
	if !ok {
		return "", errors.New("unknown revision " + ref)
	}
	return sha, nil
}
func (f *fakePushGit) IsAncestor(a, d string) (bool, error) {
	if a != "aaa" || d != "bbb" {
		return false, errors.New("ancestry asked about the wrong commits")
	}
	return f.ancestor, f.ancestorEr
}

// A rejected push whose remote is ahead of the local tip is a seat started
// again after its sandbox pushed its own work: it counts as pushed. Everything
// else keeps the push's own error, the same value, and nothing is ever forced
// (gt-gii34).
func TestPushOrAcceptRemoteAhead(t *testing.T) {
	t.Parallel()
	// The ref line git prints when a push is refused as non-fast-forward.
	rejected := errors.New(" ! [rejected]        b -> b (fetch first)")
	unreachable := errors.New("fatal: unable to access remote: Could not resolve host")
	revs := map[string]string{"refs/heads/b": "aaa", "refs/remotes/origin/b": "bbb"}
	cases := []struct {
		name         string
		git          fakePushGit
		wantAccepted bool
	}{
		{"accepted", fakePushGit{}, true},
		{"remote_ahead", fakePushGit{pushErr: rejected, revs: revs, ancestor: true}, true},
		{"diverged", fakePushGit{pushErr: rejected, revs: revs, ancestor: false}, false},
		{"no_remote_branch", fakePushGit{pushErr: rejected, fetchErr: errors.New("couldn't find remote ref"), revs: revs, ancestor: true}, false},
		{"no_remote_tracking_ref", fakePushGit{pushErr: rejected, revs: map[string]string{"refs/heads/b": "aaa"}, ancestor: true}, false},
		{"no_local_branch", fakePushGit{pushErr: rejected, revs: map[string]string{"refs/remotes/origin/b": "bbb"}, ancestor: true}, false},
		{"ancestry_error", fakePushGit{pushErr: rejected, revs: revs, ancestorEr: errors.New("git failed")}, false},
		{"not_a_rejection", fakePushGit{pushErr: unreachable, revs: revs, ancestor: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := tc.git
			err := pushOrAcceptRemoteAhead(&g, "b")
			if tc.wantAccepted {
				if err != nil {
					t.Fatalf("error = %v, want the push accepted", err)
				}
			} else if err != tc.git.pushErr {
				t.Fatalf("error = %v, want the push's own error %v", err, tc.git.pushErr)
			}
			if g.forced {
				t.Error("a force push was asked for")
			}
		})
	}
}

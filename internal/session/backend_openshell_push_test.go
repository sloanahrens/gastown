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
// else keeps the push's error, and nothing is ever forced (gt-gii34).
func TestPushOrAcceptRemoteAhead(t *testing.T) {
	t.Parallel()
	rejected := errors.New("rejected: non-fast-forward")
	revs := map[string]string{"refs/heads/b": "aaa", "refs/remotes/origin/b": "bbb"}
	cases := []struct {
		name    string
		git     fakePushGit
		wantErr bool
	}{
		{"accepted", fakePushGit{}, false},
		{"remote_ahead", fakePushGit{pushErr: rejected, revs: revs, ancestor: true}, false},
		{"diverged", fakePushGit{pushErr: rejected, revs: revs, ancestor: false}, true},
		{"no_remote_branch", fakePushGit{pushErr: rejected, fetchErr: errors.New("couldn't find remote ref"), revs: revs, ancestor: true}, true},
		{"no_remote_tracking_ref", fakePushGit{pushErr: rejected, revs: map[string]string{"refs/heads/b": "aaa"}, ancestor: true}, true},
		{"no_local_branch", fakePushGit{pushErr: rejected, revs: map[string]string{"refs/remotes/origin/b": "bbb"}, ancestor: true}, true},
		{"ancestry_error", fakePushGit{pushErr: rejected, revs: revs, ancestorEr: errors.New("git failed")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := tc.git
			err := pushOrAcceptRemoteAhead(&g, "b")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && tc.git.pushErr != nil && err != rejected {
				t.Errorf("error = %v, want the push's own error", err)
			}
			if g.forced {
				t.Error("a force push was asked for")
			}
		})
	}
}

package session

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakePushGit scripts the git calls of the push step and records whether a
// force was asked for. The first push returns pushErr; a later push returns
// retryErr. Ancestry and counts are scripted per pair, and the two heads the
// push step reads are the fixed shas aaa (local) and bbb (origin).
type fakePushGit struct {
	pushErr     error
	retryErr    error
	pushes      int
	fetchErr    error
	revs        map[string]string  // ref -> sha; a missing ref is an error
	ancestors   map[[2]string]bool // ancestors[{a, d}]: a is an ancestor of d
	ancestorEr  error
	mergeBase   string
	mergeBaseEr error
	localOnly   int // commits aaa holds that bbb lacks
	remoteOnly  int // commits bbb holds that aaa lacks
	countEr     error
	// remoteCountEr is the failure of the count of commits bbb holds that aaa
	// lacks, so the two counts can fail one at a time.
	remoteCountEr error
	forced        bool
}

func (f *fakePushGit) Push(_, _ string, force bool) error {
	f.forced = f.forced || force
	f.pushes++
	if f.pushes == 1 {
		return f.pushErr
	}
	return f.retryErr
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
	return f.ancestors[[2]string{a, d}], f.ancestorEr
}
func (f *fakePushGit) MergeBase(string, string) (string, error) {
	return f.mergeBase, f.mergeBaseEr
}
func (f *fakePushGit) CommitsAhead(base, branch string) (int, error) {
	switch {
	case base == "bbb" && branch == "aaa":
		return f.localOnly, f.countEr
	case base == "aaa" && branch == "bbb":
		return f.remoteOnly, f.remoteCountEr
	}
	return 0, errors.New("counted commits between the wrong heads " + base + ".." + branch)
}

// A rejected push of a branch origin holds commits for is classified before
// anything is decided. A behind or equal origin counts as pushed, since the
// clone fetches what origin holds. A local head that is ahead retries the plain
// push. A diverged branch is refused with the divergence named, and nothing is
// ever forced (gt-gii34, gt-0361s2).
func TestPushOrAcceptRemoteAhead(t *testing.T) {
	t.Parallel()
	// git's refusal for a non-fast-forward push, over the lines git prints.
	rejected := errors.New("To http://forgejo:3000/sloan/gastown.git\n ! [rejected]        b -> b (non-fast-forward)\nerror: failed to push some refs\nhint: Updates were rejected because the tip of your current branch is behind")
	retryRefused := errors.New(" ! [rejected]        b -> b (fetch first)")
	unreachable := errors.New("fatal: unable to access remote: Could not resolve host")
	revs := map[string]string{"refs/heads/b": "aaa", "refs/remotes/origin/b": "bbb"}
	behind := map[[2]string]bool{{"aaa", "bbb"}: true}
	ahead := map[[2]string]bool{{"bbb", "aaa"}: true}
	cases := []struct {
		name         string
		git          fakePushGit
		wantAccepted bool
		wantSame     error    // the error returned as it is
		wantText     []string // the error names each of these, on one line
	}{
		{"accepted", fakePushGit{}, true, nil, nil},
		{"equal", fakePushGit{pushErr: rejected, revs: map[string]string{"refs/heads/b": "aaa", "refs/remotes/origin/b": "aaa"}}, true, nil, nil},
		{"behind", fakePushGit{pushErr: rejected, revs: revs, ancestors: behind}, true, nil, nil},
		{"ahead_push_goes_through", fakePushGit{pushErr: rejected, revs: revs, ancestors: ahead}, true, nil, nil},
		{"ahead_push_refused_again", fakePushGit{pushErr: rejected, retryErr: retryRefused, revs: revs, ancestors: ahead}, false, retryRefused, nil},
		{"diverged", fakePushGit{pushErr: rejected, revs: revs, mergeBase: "mmm", localOnly: 2, remoteOnly: 1}, false, nil,
			[]string{"branch b diverged from origin", "local aaa", "origin bbb", "merge-base mmm", "local holds 2 commits origin lacks", "origin holds 1 local lacks", "error: failed to push some refs", "hint: Updates were rejected", "nothing is forced"}},
		{"diverged_base_unreadable", fakePushGit{pushErr: rejected, revs: revs, mergeBaseEr: errors.New("no merge base")}, false, nil,
			[]string{"branch b diverged from origin", "divergence could not be read (no merge base)", "[rejected]", "nothing is forced"}},
		{"diverged_local_count_unreadable", fakePushGit{pushErr: rejected, revs: revs, mergeBase: "mmm", countEr: errors.New("rev-list failed")}, false, nil,
			[]string{"branch b diverged from origin", "divergence could not be read (rev-list failed)", "[rejected]", "nothing is forced"}},
		{"diverged_remote_count_unreadable", fakePushGit{pushErr: rejected, revs: revs, mergeBase: "mmm", remoteCountEr: errors.New("rev-list failed")}, false, nil,
			[]string{"branch b diverged from origin", "divergence could not be read (rev-list failed)", "[rejected]", "nothing is forced"}},
		{"no_remote_branch", fakePushGit{pushErr: rejected, fetchErr: errors.New("couldn't find remote ref"), revs: revs, ancestors: behind}, false, rejected, nil},
		{"no_remote_tracking_ref", fakePushGit{pushErr: rejected, revs: map[string]string{"refs/heads/b": "aaa"}, ancestors: behind}, false, rejected, nil},
		{"no_local_branch", fakePushGit{pushErr: rejected, revs: map[string]string{"refs/remotes/origin/b": "bbb"}, ancestors: behind}, false, rejected, nil},
		{"ancestry_error", fakePushGit{pushErr: rejected, revs: revs, ancestorEr: errors.New("git failed")}, false, rejected, nil},
		{"not_a_rejection", fakePushGit{pushErr: unreachable, revs: revs, ancestors: behind}, false, unreachable, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := tc.git
			err := pushOrAcceptRemoteAhead(&g, "b")
			switch {
			case tc.wantAccepted:
				if err != nil {
					t.Fatalf("error = %v, want the push accepted", err)
				}
			case tc.wantSame != nil:
				if err != tc.wantSame {
					t.Fatalf("error = %v, want %v as it is", err, tc.wantSame)
				}
			default:
				if err == nil {
					t.Fatal("error = nil, want the diverged branch refused")
				}
				if strings.Contains(err.Error(), "\n") {
					t.Errorf("diverged refusal spans lines: %q", err.Error())
				}
				for _, want := range tc.wantText {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("diverged refusal = %q, want it to name %q", err.Error(), want)
					}
				}
			}
			if g.forced {
				t.Error("a force push was asked for")
			}
		})
	}
}

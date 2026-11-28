package land

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// gateAheadHeadRepo is the fixture's Repo with the head check's calls failing
// on demand; it records whether each fetch ran under the lander's git lock.
type gateAheadHeadRepo struct {
	Repo
	fetchErr, branchFetchErr, tipErr error
	lockHeld                         func() bool
	fetchLocks                       []bool
}

func (r *gateAheadHeadRepo) FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error {
	if r.lockHeld != nil {
		r.fetchLocks = append(r.fetchLocks, r.lockHeld())
	}
	switch {
	case r.fetchErr != nil && strings.Contains(refspec, "refs/heads/main:"):
		return r.fetchErr
	case r.branchFetchErr != nil && strings.Contains(refspec, fixtureBranch):
		return r.branchFetchErr
	}
	return r.Repo.FetchRefspecWithTimeout(remote, refspec, timeout)
}

func (r *gateAheadHeadRepo) PushRemoteBranchTip(remote, branch string) (string, error) {
	if r.tipErr != nil {
		return "", r.tipErr
	}
	return r.Repo.PushRemoteBranchTip(remote, branch)
}

// TestGateAheadHeadCheck: a pushed head passes, and each failure the check can
// meet returns an error naming it with no bead write.
func TestGateAheadHeadCheck(t *testing.T) {
	t.Parallel()
	boom := errors.New("git read failed")
	tests := []struct {
		name    string
		setup   func(t *testing.T, f *landFixture, r *gateAheadHeadRepo)
		wantErr string
	}{
		{"pushed head passes", nil, ""},
		{"failed target fetch", func(t *testing.T, f *landFixture, r *gateAheadHeadRepo) { r.fetchErr = boom }, "fetch target"},
		{"failed branch fetch", func(t *testing.T, f *landFixture, r *gateAheadHeadRepo) { r.branchFetchErr = boom }, "fetch branch"},
		{"failed branch tip read", func(t *testing.T, f *landFixture, r *gateAheadHeadRepo) { r.tipErr = boom }, "read branch tip"},
		{"head not on the branch", func(t *testing.T, f *landFixture, r *gateAheadHeadRepo) {
			f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, f.base)
		}, "not reachable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l := f.lander()
			r := &gateAheadHeadRepo{Repo: f.git.Open(f.repo)}
			if tt.setup != nil {
				tt.setup(t, f, r)
			}
			before := aheadBeadWrites(t, f.bd)
			err := l.gateAheadHeadCheck(r, f.work)
			if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
				t.Errorf("wrote %d bead records, want 0", n)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("gateAheadHeadCheck: %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("gateAheadHeadCheck = %v, want an error naming %q", err, tt.wantErr)
			}
		})
	}
}

// TestGateAheadHeadCheckTakesTheGitLock: the target fetch runs under the
// lander's git lock, as Land's does.
func TestGateAheadHeadCheckTakesTheGitLock(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	r := &gateAheadHeadRepo{Repo: f.git.Open(f.repo), lockHeld: l.GitLockHeld}
	if err := l.gateAheadHeadCheck(r, f.work); err != nil {
		t.Fatalf("gateAheadHeadCheck: %v", err)
	}
	if len(r.fetchLocks) == 0 || !r.fetchLocks[0] {
		t.Errorf("the target fetch ran outside the git lock; fetch lock states = %v", r.fetchLocks)
	}
}

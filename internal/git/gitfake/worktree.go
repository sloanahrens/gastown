package gitfake

import (
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// WorkTree is the part of *git.Git the fake answers for internal/daemon
// beyond Repo: commit times so far. Open's result implements it; a consumer
// asserts it from Repo. RunWorkTreeContract pins it to real git.
type WorkTree interface {
	CommitTime(rev string) (time.Time, error)
}

var (
	_ WorkTree = (*git.Git)(nil)
	_ WorkTree = (*handle)(nil)
)

// commitEpoch is the committer time of the fake's commit number zero; commit
// n is n seconds later, the dates the integration tier's fixtures give the
// commits they make.
const commitEpoch = 1_700_000_000

// CommitTime returns rev's committer time.
func (h *handle) CommitTime(rev string) (time.Time, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"show", "-s", "--format=%cI", rev}
	r, wt, err := h.locate(args...)
	if err != nil {
		return time.Time{}, err
	}
	id, ok := h.resolve(r, wt, rev)
	if !ok || h.f.objects[id] == nil {
		return time.Time{}, gitErr(128, "fatal: ambiguous argument '"+rev+"': unknown revision or path not in the working tree.", args...)
	}
	return time.Unix(int64(commitEpoch+h.f.objects[id].seq), 0).UTC(), nil
}

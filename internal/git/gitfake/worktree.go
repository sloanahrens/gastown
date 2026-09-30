package gitfake

import (
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// WorkTree is the part of *git.Git the fake answers for internal/daemon
// beyond Repo: commit times, the staging model (index.go), and the tree and
// blob reads git.DetectRevertedMerges makes. Open's result implements it; a
// consumer asserts it from Repo. RunWorkTreeContract pins it to real git.
type WorkTree interface {
	CommitTime(rev string) (time.Time, error)

	Status() (*git.GitStatus, error)
	Add(pathspecs ...string) error
	ResetFiles(pathspecs ...string) error
	StagedChanges() ([]git.StagedChange, error)
	WriteTree() (string, error)
	Commit(message string) error
	CurrentBranch() (string, error)
	GetUpstreamURL() (string, error)
	CleanDefaultBranchBaseRef(remote, defaultBranch string) string

	// Pushing and preservation (push.go).
	Push(remote, refspec string, force bool) error
	StashCount() (int, error)
	CheckUncommittedWorkLocalFailClosed() (*git.UncommittedWorkStatus, error)

	// The backup repository (backup.go).
	InitRepo(branch string) error
	ConfigSet(key, value string) error
	ConfigGet(key string) (string, error)
	CommitWithAuthor(message, author string) error
	PackSize() (string, error)
	LogAll(max int) ([]git.LogEntry, error)

	git.RevertReader
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
	return commitTime(h.f.objects[id]), nil
}

package polecat

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// resumeFetchLog collects the bounds of the branch fetches a manager makes.
// When err is set, every fetch it records fails with it instead of running.
type resumeFetchLog struct {
	timeouts []time.Duration
	err      error
}

// boundedFetchRepo records the bound of every FetchBranchWithTimeout call on
// the repository it wraps.
type boundedFetchRepo struct {
	gitRepo
	log *resumeFetchLog
}

func (r boundedFetchRepo) FetchBranchWithTimeout(remote, branch string, timeout time.Duration) error {
	r.log.timeouts = append(r.log.timeouts, timeout)
	if r.log.err != nil {
		return r.log.err
	}
	return r.gitRepo.FetchBranchWithTimeout(remote, branch, timeout)
}

// recordResumeFetchesOn makes every repository mgr opens record its bounded
// branch fetches on log.
func recordResumeFetchesOn(mgr *Manager, w *world, log *resumeFetchLog) {
	base := w.opener()
	mgr.gits = gitOpener{
		open: func(dir string) gitRepo {
			return boundedFetchRepo{base.open(dir), log}
		},
		openDir: func(gitDir, workDir string) gitRepo {
			return boundedFetchRepo{base.openDir(gitDir, workDir), log}
		},
		repoFile: base.repoFile,
	}
}

// TestAddNamedWithOptions_ResumeFetchUsesTheBranchFetchBound pins gt-nh02an: the
// create path's resume fetch is a whole-branch fetch, so it carries the
// resume bound, not the remote-query bound FetchBranch applies.
func TestAddNamedWithOptions_ResumeFetchUsesTheBranchFetchBound(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, _, w := canonicalRig(t)

	branch := "polecat/jasper/gt-bagu+mudcl5gv"
	mainSHA := w.rev(t, mayorRig, "origin/main")
	w.branchAtNewCommit(t, mayorRig, branch, mainSHA, "bagu work (gt-bagu)")

	log := &resumeFetchLog{}
	recordResumeFetchesOn(mgr, w, log)

	if _, err := mgr.AddNamedWithOptions("gamma", AddOptions{HookBead: "gt-next", ResumeBranch: branch}); err != nil {
		t.Fatalf("AddNamedWithOptions: %v", err)
	}
	if len(log.timeouts) != 1 {
		t.Fatalf("bounded branch fetches = %d (%v), want 1 for the resume", len(log.timeouts), log.timeouts)
	}
	if got := log.timeouts[0]; got <= git.RemoteQueryTimeout {
		t.Errorf("resume fetch bound = %v, want wider than the %v remote-query bound FetchBranch applies", got, git.RemoteQueryTimeout)
	}
}

// TestAddNamedWithOptions_ResumeFetchFailureContinuesFromLocalRefs pins the
// fallback gt-nh02an keeps: a resume fetch that fails (offline, or timed out at
// the bound) is not fatal, and the polecat starts on the branch tip already in
// the repository.
func TestAddNamedWithOptions_ResumeFetchFailureContinuesFromLocalRefs(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, _, w := canonicalRig(t)

	branch := "polecat/jasper/gt-bagu+mudcl5gv"
	mainSHA := w.rev(t, mayorRig, "origin/main")
	tip := w.branchAtNewCommit(t, mayorRig, branch, mainSHA, "bagu work (gt-bagu)")

	log := &resumeFetchLog{err: errors.New("fetch: timed out")}
	recordResumeFetchesOn(mgr, w, log)

	p, err := mgr.AddNamedWithOptions("gamma", AddOptions{HookBead: "gt-next", ResumeBranch: branch})
	if err != nil {
		t.Fatalf("AddNamedWithOptions with a failed resume fetch: %v", err)
	}
	if len(log.timeouts) != 1 {
		t.Fatalf("bounded branch fetches = %d (%v), want 1 for the resume", len(log.timeouts), log.timeouts)
	}

	worktreeGit := w.repo(p.ClonePath)
	current, err := worktreeGit.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if current != branch {
		t.Fatalf("worktree HEAD on branch %q, want %q", current, branch)
	}
	reachable, err := worktreeGit.IsAncestor(tip, "HEAD")
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if !reachable {
		t.Fatalf("branch tip %s reachable from HEAD = false, want the local tip after a failed resume fetch", tip)
	}
}

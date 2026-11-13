package polecat

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// resumeFetchLog collects the bounds of the branch fetches a manager makes.
type resumeFetchLog struct {
	timeouts []time.Duration
}

// boundedFetchRepo records the bound of every FetchBranchWithTimeout call on
// the repository it wraps.
type boundedFetchRepo struct {
	gitRepo
	log *resumeFetchLog
}

func (r boundedFetchRepo) FetchBranchWithTimeout(remote, branch string, timeout time.Duration) error {
	r.log.timeouts = append(r.log.timeouts, timeout)
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
// resume bound, not the 30 seconds FetchBranch applies to a remote query.
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

//go:build integration

package version

import (
	"strings"
	"testing"
)

// TestIntegrationCheckStaleBareRepoAnswersFromItsOriginMain runs the staleness
// check over a rig's bare repo, the .repo.git the landing worker refreshes,
// with real git. The bare repo's HEAD is refs/heads/main, as the rig sets it,
// but nothing writes that branch: the verdict comes from
// refs/remotes/origin/main, which is current, even while refs/heads/main is
// stale.
func TestIntegrationCheckStaleBareRepoAnswersFromItsOriginMain(t *testing.T) {
	t.Parallel()
	_, bare, _, tip := detachedRig(t)
	detachedGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	// The bare's refs/heads/main is already the stale old commit: nothing writes
	// it. Only refs/remotes/origin/main is current.
	old := detachedGit(t, bare, "rev-parse", "refs/remotes/origin/main~1")

	info := CheckStaleBinaryOf(bare, old)

	if info.Error != nil || info.Skipped {
		t.Fatalf("Error=%v Skipped=%v (%s), want an answer from the bare repo", info.Error, info.Skipped, info.SkipReason)
	}
	if info.CompareRef != "origin/main" {
		t.Errorf("CompareRef = %q, want origin/main", info.CompareRef)
	}
	if info.RepoCommit != tip {
		t.Errorf("RepoCommit = %s, want origin/main's tip %s", info.RepoCommit, tip)
	}
	if !info.IsStale || info.CommitsBehind != 1 {
		t.Errorf("IsStale=%v CommitsBehind=%d, want stale by one commit", info.IsStale, info.CommitsBehind)
	}
}

// TestIntegrationCheckStaleBareRepoWithoutOriginMainIsAnError pins the refusal:
// a bare repo with no refs/remotes/origin/main has no main to compare against,
// and the check says so instead of reading the writerless refs/heads/main.
func TestIntegrationCheckStaleBareRepoWithoutOriginMainIsAnError(t *testing.T) {
	t.Parallel()
	_, bare, _, tip := detachedRig(t)
	detachedGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	detachedGit(t, bare, "update-ref", "refs/heads/main", tip)
	detachedGit(t, bare, "update-ref", "-d", "refs/remotes/origin/main")

	info := CheckStaleBinaryOf(bare, tip)

	if info.Error == nil || !strings.Contains(info.Error.Error(), "refs/remotes/origin/main") {
		t.Fatalf("Error = %v, want one naming refs/remotes/origin/main", info.Error)
	}
	if info.CompareRef != "" {
		t.Errorf("CompareRef = %q, want none without origin/main", info.CompareRef)
	}
}

//go:build integration

package version

import "testing"

// TestIntegrationCheckStaleBareRepoAnswersFromItsMain runs the staleness check
// over a rig's bare repo, the .repo.git the landing worker refreshes, with real
// git. The bare repo's HEAD is refs/heads/main, as the rig sets it, and it has
// no origin/main: the verdict comes from main alone.
func TestIntegrationCheckStaleBareRepoAnswersFromItsMain(t *testing.T) {
	t.Parallel()
	_, bare, _, tip := detachedRig(t)
	detachedGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	binary := detachedGit(t, bare, "rev-parse", "main~1")

	info := CheckStaleBinaryOf(bare, binary)

	if info.Error != nil || info.Skipped {
		t.Fatalf("Error=%v Skipped=%v (%s), want an answer from the bare repo", info.Error, info.Skipped, info.SkipReason)
	}
	if info.CompareRef != "main" {
		t.Errorf("CompareRef = %q, want main", info.CompareRef)
	}
	if info.RepoCommit != tip {
		t.Errorf("RepoCommit = %s, want main's tip %s", info.RepoCommit, tip)
	}
	if !info.IsStale || info.CommitsBehind != 1 {
		t.Errorf("IsStale=%v CommitsBehind=%d, want stale by one commit", info.IsStale, info.CommitsBehind)
	}
}

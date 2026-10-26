package land

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// envValue is the value env carries for key, if any.
func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, prefix); ok {
			return v, true
		}
	}
	return "", false
}

// A landing commit changes the committer's name and nothing else: the email
// and the author stay the repository's and the range's (gt-dc4y3).
func TestCommittingEnvSetsOnlyTheCommitterName(t *testing.T) {
	t.Parallel()
	env := CommittingEnv()
	if len(env) != 1 || env[0] != "GIT_COMMITTER_NAME="+LandingCommitterName {
		t.Fatalf("CommittingEnv = %q, want only GIT_COMMITTER_NAME=%s", env, LandingCommitterName)
	}
	for _, key := range []string{"GIT_COMMITTER_EMAIL", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL"} {
		if v, ok := envValue(env, key); ok {
			t.Errorf("%s = %q; a landing sets no committer email and no author", key, v)
		}
	}
}

// The merge candidate is committed by the Git the lander opens for its
// worktree, so that handle hands git the landing committer (gt-dc4y3).
func TestLandMergeWorktreeCarriesTheLandingCommitter(t *testing.T) {
	t.Parallel()
	opened := (&Lander{}).open(t.TempDir())
	g, ok := opened.(*git.Git)
	if !ok {
		t.Fatalf("Lander.open returned %T, want *git.Git", opened)
	}
	if v, _ := envValue(g.Environ(), "GIT_COMMITTER_NAME"); v != LandingCommitterName {
		t.Fatalf("merge worktree GIT_COMMITTER_NAME = %q, want %q", v, LandingCommitterName)
	}
}
